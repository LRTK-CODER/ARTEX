package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/report"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// BuildVersion is the backend application version, injected from cmd/artex at
// startup (which in turn gets it from -ldflags "-X main.version=<tag>").
// Defaults to "dev" for local builds. Exposed to the frontend via GET /api/health.
var BuildVersion = "dev"

// Server exposes the ARTEX backend over a JSON HTTP API for the shadcn/ui
// frontend.
type Server struct {
	m      *Manager
	engine *Engine
	ctx    context.Context

	skillDir string // root directory for skill subdirectories
	jwtKey   []byte // keyDir/jwt.key 에서 읽거나 새로 만든 HS256 서명 키
	// oauth 는 구독 OAuth 프로필의 TokenSource를 프로필마다 하나씩 들고 있다.
	// oauth.key를 불러오지 못했으면 nil 이고, 그때 OAuth 프로필은 쓸 수 없다.
	oauth *oauthTokenRegistry
	// codexBaseURL 은 chatgpt_oauth 프로필이 부르는 Codex 백엔드 주소다. 비면 agent.CodexBaseURL 이다.
	// 테스트만 httptest 주소로 바꾼다. 설정·환경 변수로 바꾸는 길은 두지 않는다(codexURL).
	codexBaseURL string
	// claudeBaseURL 은 내부 테스트에서만 바꾸며 설정·환경 변수를 따르지 않는다.
	claudeBaseURL string
	// loginFlows 는 진행 중인 ChatGPT 구독 로그인 흐름이다. 메모리에만 둔다(chatgpt_login.go).
	loginFlows loginFlows
	// claudeLoginFlows 는 Claude 로그인 흐름이며 연결 해제 전까지 메모리에만 둔다.
	claudeLoginFlows loginFlows

	// concMu serializes concurrency-cap decisions (admission + reconcile) so a
	// scheduler tick and an HTTP settings change / task creation can't both count
	// free slots off the same snapshot and over-promote past the limit.
	concMu sync.Mutex

	cfgMu     sync.Mutex
	chatAgent *agent.ChatAgent // conversational runner for the chat page; nil w/o LLM
	// llmProv is the fully-decorated global provider (recorder + failover chain)
	// installed by applyLLM. Task routers use it after an explicit chain is cleared.
	llmProv   llm.Provider
	llmDirect llm.Provider // concrete global provider, before any failover pool
	llmCfg    agent.Config // current LLM config (key not exposed)
	llmOn     bool
	llmProf   string // active LLM profile name (for llmrec tagging)

	// chatBusy guards the per-task main-agent run: the chat handler launches the
	// agent on the server's background ctx (not the request ctx) and returns
	// immediately, so a page reload / proxy timeout can't abort a live run. This
	// map serializes turns per task — one main-agent run at a time per task, so
	// concurrent messages don't corrupt the shared exp<id>-main transcript.
	chatMu   sync.Mutex
	chatBusy map[string]bool
	// chatCancel holds the cancel func for each in-flight conversation run (keyed by
	// convBusyKey), so a manual stop can abort JUST that session's agent run. Set/
	// cleared alongside chatBusy under chatMu. Aborting a run does NOT touch the P3
	// trigger queue — the drain goroutine simply proceeds to the next queued fire.
	chatCancel map[string]context.CancelCauseFunc

	// triggerQ는 P3 트리거 실행을 에이전트별로 쌓아 둔다. 에이전트마다 하나인 "pump"가
	// 에이전트 정책에서 나온 동시 실행 제한까지 실행을 띄운다: serial → 제한 1(+ 선택적
	// 병합), parallel → 제한 = trigger_max_parallel(0=∞), 병합 없음. triggerActive는
	// 에이전트별 진행 중 실행 수를 센다(불리언 drain 플래그를 대신한다). 실행이 끝나면 수를
	// 줄이고 다시 pump해 빈자리를 채운다. triggerCfg는 에이전트 정책을 마지막으로 읽은 값을
	// 캐시해 pump가 queueMu를 잡은 채 DB를 조회하지 않게 한다. 서로 다른 에이전트는 늘
	// 동시에 돈다. 큐는 메모리에만 있으므로(chatBusy와 같다) 재시작하면 대기 중인 실행이
	// 사라지고, 스케줄러가 다음 tick에 워터마크부터 다시 실행한다.
	queueMu       sync.Mutex
	triggerQ      map[string][]triggeredRun
	triggerActive map[string]int
	triggerCfg    map[string]triggerBehavior

	// profChatAgents caches a per-profile ChatAgent (chat page), keyed by profile id.
	// Built lazily on first use; invalidated when any profile is saved/activated/deleted
	// so edits take effect.
	profMu         sync.Mutex
	profChatAgents map[int64]*agent.ChatAgent // per-profile ChatAgent cache (chat page)

	// provByProfile caches ONE provider per LLM profile id so every agent bound or
	// pinned to the same profile shares a single provider instance — hence one rate
	// limiter. applyLLM also resolves the persisted global-active profile through this
	// cache, so global fallback and task chains do not accidentally double the
	// configured request rate. Cleared on profile edits, then repopulated by active reapply.
	provCacheMu   sync.Mutex
	provByProfile map[int64]*provEntry
	provCacheGen  uint64

	// llmHealth 는 LLM 장애 조치용 프로세스 전역 회로 차단기 상태다. 일부러 provider
	// 캐시 밖에 둔다: 체인을 다시 만들어도(관련 없는 프로필 저장, 설정 전환) 어느
	// 백엔드가 크레딧 부족·속도 제한 상태인지 알아낸 것을 지우면 안 되기 때문이다.
	llmHealth *llmpool.Registry

	// Explicit task chains use one task-scoped dynamic provider shared by goals,
	// planner, workers, and the main agent. Bundles are stable; their runtime reads
	// the persisted current profile at every new LLM call.
	taskAgentMu sync.Mutex
	taskAgents  map[string]*taskAgentBundle

	// Cold task archives run through one persistent FIFO worker. The buffered wake
	// channel coalesces enqueue bursts; the database remains the source of truth.
	archiveWake chan struct{}
	archiveWG   sync.WaitGroup
	side        *sideQuestionState
}

// provEntry is a cached provider + its config for one LLM profile id.
type provEntry struct {
	prov llm.Provider
	cfg  agent.Config
}

// triggeredRun is one queued P3 trigger fire awaiting its turn for an agent.
// taskID + mergeable let the drainer coalesce several event triggers (finding/goal)
// from the SAME task into one conversation before it starts (interval fires don't merge).
type triggeredRun struct {
	agentKey  string
	title     string
	message   string // 이벤트 본문(트리거 문구 + 도구·입력·반환 등). 작업 설명·목표 머리말은 넣지 않는다
	taskID    int64  // source task for finding/goal triggers; 0 for interval/none
	taskDesc  string // 작업 설명(작업 단위라 같은 작업이면 같다). 병합할 때 한 번만 렌더링한다
	taskGoal  string // 작업 목표(작업 단위라 같은 작업이면 같다). 병합할 때 한 번만 렌더링한다
	mergeable bool   // true for finding/goal event triggers (merge by taskID)
}

func New(ctx context.Context, m *Manager, skillDir string, dataDir string, keyDir string) *Server {
	key, err := loadOrCreateJWTKey(keyDir, dataDir)
	if err != nil {
		log.Fatalf("[auth] JWT key: %v", err)
	}
	s := &Server{m: m, engine: NewEngine(m), ctx: ctx, skillDir: skillDir, jwtKey: key, chatBusy: map[string]bool{},
		chatCancel: map[string]context.CancelCauseFunc{}, triggerQ: map[string][]triggeredRun{},
		triggerActive: map[string]int{}, triggerCfg: map[string]triggerBehavior{},
		profChatAgents: map[int64]*agent.ChatAgent{},
		provByProfile:  map[int64]*provEntry{}, llmHealth: newLLMHealthRegistry(m.pg),
		taskAgents: map[string]*taskAgentBundle{}, archiveWake: make(chan struct{}, 1)}
	// OAuth 토큰 키는 jwt.key와 같은 키 디렉터리에 둔다(파일 관리자로 열람할 수 없는 곳).
	s.oauth = loadOAuthTokenRegistry(m.pg, keyDir)
	s.initSideQuestions()
	// 회로 차단기 임계값·대기 시간은 실패 경로에서 자주 읽는 값이라, 시작할 때 전역 재시도
	// 정책을 Registry에 한 번 밀어 넣고 이후 정책을 저장할 때마다 다시 밀어 넣는다(saveLLMRetryPolicy).
	s.applyRetryPolicy()
	// Every task uses a stable task router. An empty explicit chain is resolved by
	// that router through Agent bindings and then the global provider, so adding a
	// first chain to a running task takes effect on its very next LLM call.
	s.engine.SetAuthoritativeAgentResolver(func(t *Task) (*agent.Planner, *agent.Worker) {
		if !s.taskRuntimeAvailable(t, "planner", "worker") {
			return nil, nil
		}
		b := s.agentsForTask(t)
		return b.pl, b.wk
	})
	// Global readiness (Ready()/llm_configured): a global active LLM provider is
	// installed. Task-level runnability is separate (ReadyFor → the resolver above).
	s.engine.SetReadiness(func() bool {
		s.cfgMu.Lock()
		defer s.cfgMu.Unlock()
		return s.llmOn
	})
	// DB에 저장된 프롬프트 템플릿을 에이전트에 배선한다. 덮어쓰는 행이 없으면 에이전트는
	// 내장 기본값을 그대로 쓴다(동작은 바뀌지 않는다).
	if m.pg != nil {
		agent.PromptOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil {
				return "", false
			}
			t, err := m.pg.CurrentPrompt(a.ID)
			if err != nil || t == "" {
				return "", false
			}
			return t, true
		}
		// Wire DB-stored wrap-up (settlement) prompts. Empty column → built-in default.
		agent.WrapupOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.WrapupPrompt == "" {
				return "", false
			}
			return a.WrapupPrompt, true
		}
		// Wire DB-stored wrap-up turn budgets. 0 / missing → built-in per-agent default.
		agent.WrapupMaxTurnsOverride = func(key string) (int, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.WrapupMaxTurns <= 0 {
				return 0, false
			}
			return a.WrapupMaxTurns, true
		}
		// Wire DB-stored TASK-TIMEOUT wrap-up prompt/turns (worker/planner). Empty/0 → default.
		agent.WrapupTaskTimeoutOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.TaskTimeoutWrapupPrompt == "" {
				return "", false
			}
			return a.TaskTimeoutWrapupPrompt, true
		}
		agent.WrapupTaskTimeoutTurnsOverride = func(key string) (int, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.TaskTimeoutWrapupMaxTurns <= 0 {
				return 0, false
			}
			return a.TaskTimeoutWrapupMaxTurns, true
		}
		wireAgentAugment(m.pg, s.skillDir, s.hostTools) // 공개된 스킬·MCP와 트래픽·오케스트레이션 host 도구를 에이전트 도구 모음에 넣는다
		domainReg := buildDomainReg(m.Assets())
		wireTools(m.pg, domainReg) // 내장 도구 표: 에이전트별 필터 + 설명·schema 덮어쓰기 + 기본값 넣기
		seedPrompts(m.pg)          // 내장 에이전트 기본 프롬프트 본문을 agent_prompts에 시드한다(비어 있을 때만)
		s.seedOrchestrationTools() // P2 작업 간 오케스트레이션 도구를 tools 표에 시드한다(에이전트별로 연결할 수 있다)
		if err := s.seedFindingRetester(); err != nil {
			log.Printf("[retester] seed: %v", err)
		}
		go s.evidenceStore().RunGC(s.ctx)
		s.seedPythonInterpreter()     // 사용자 지정 스크립트 도구: 시작할 때 python 인터프리터를 찾아 저장한다(비어 있을 때만)
		go newScheduler(s).Run(s.ctx) // P3 트리거 스케줄링(주기·finding·목표 이벤트). 사용자 지정 에이전트만
		// 취약점 IM 알림 전달 엔진. Scheduler와 나란히 돌지만 독립적이다: 알림의 실시간성
		// 요구(3s)가 트리거의 업무 주기와 다르고, 한쪽 실패가 다른 쪽에 번지면 안 된다.
		// 알림 발송이 멈춰도 에이전트 트리거에는 영향이 없어야 한다.
		go newNotifier(s).Run(s.ctx)
		// Fill the tool cache for any enabled MCP that has none yet (notably the
		// seeded browser MCP on first run). Async so it never blocks startup.
		go s.discoverEmptyMCPsOnStartup()
		logSink.SetDB(ctx, m.pg) // restore last 100 log rows and enable async persistence
	}
	// precedence: persisted DB config > env.
	if cfg, ok := s.loadLLMConfig(); ok {
		if err := s.applyLLM(cfg); err != nil {
			log.Printf("[engine] saved LLM config init failed — engine idle: %v", err)
		} else {
			log.Printf("[engine] LLM configured from DB: %s / %s", cfg.Provider(), cfg.Model)
		}
	} else if cfg, ok := agent.FromEnv(); ok {
		if err := s.applyLLM(cfg); err != nil {
			log.Printf("[engine] env provider init failed — engine idle: %v", err)
		} else {
			log.Printf("[engine] LLM configured from env: %s / %s", cfg.Provider(), cfg.Model)
		}
	} else {
		log.Printf("[engine] no LLM provider configured — engine idle until set via /api/llm or env")
	}
	s.restoreTaskRuntimes()
	go s.reconcileConcurrency()
	s.startTaskArchiveWorker()
	s.wireInterceptReviewer() // 모델 승인 심사: 차단 규칙에 일치하지 않은 명령을 모델이 판정한다
	return s
}

// Restored deadline and worker loops must inherit the same context as new tasks,
// including the side-question checkpoint publisher installed during startup.
func (s *Server) restoreTaskRuntimes() {
	m := s.m
	// reload tasks persisted on disk so the task list survives a restart, and
	// restore persisted paused state (so a task paused before restart stays paused).
	for _, t := range m.LoadExisting() {
		lifecycle := t.lifecycleSnapshot()
		// clear stale 'running' intents from a prior crash/restart (no live worker
		// owns them) so they re-claim instead of spinning forever in the UI.
		if n, _ := t.Store.ResetRunningIntents(); n > 0 {
			log.Printf("[engine] task %s: 남아 있던 running 의도 %d개를 open으로 되돌림", t.ID, n)
		}
		if lifecycle.Paused {
			s.engine.Pause(t.ID, agent.AbortPausedOnReload)
		}
		// 작업 단위 시간 초과: 종료 상태가 아니고 timeout이 있는 작업마다 deadline 조정자를
		// planner/worker 루프와 따로 띄운다. 그래야 활성이 아닌 작업도 재시작 뒤 시간이 되면
		// 마무리된다(deadline이 이미 지났으면 바로 마무리 절차로 간다).
		if !isTerminalStatus(lifecycle.Status) {
			s.engine.startDeadlineCoordinator(s.ctx, t)
		}
	}
	// Restore every task that had already been admitted before shutdown. Starting
	// only the active UI task left other non-queued tasks counted as concurrency
	// occupants without live loops, which could permanently block the persistent
	// FIFO. Paused loops remain idle; queued tasks are admitted below as slots allow.
	for _, t := range m.List() {
		lifecycle := t.lifecycleSnapshot()
		if !lifecycle.Queued && !isTerminalStatus(lifecycle.Status) {
			s.engine.Run(s.ctx, t)
		}
	}
}

// agentMaxTurns returns the configured max_turns for an agent key (0 = unlimited,
// also the fallback when no DB or no row).
func (s *Server) agentMaxTurns(key string) int {
	if s.m.pg == nil {
		return 0
	}
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil || a == nil {
		return 0
	}
	return a.MaxTurns
}

// agentRunSeconds returns the configured wall-clock run budget (seconds) for an
// agent key (0 = unlimited; 1200 fallback when no DB or no row, matching schema).
func (s *Server) agentRunSeconds(key string) int {
	if s.m.pg == nil {
		return 1200
	}
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil || a == nil {
		return 1200
	}
	return a.RunSecs
}

// loadLLMConfig reads the active LLM profile from PG (llm_profiles).
func (s *Server) loadLLMConfig() (agent.Config, bool) {
	p, err := s.m.pg.ActiveProfile()
	if err != nil || p == nil {
		return agent.Config{}, false
	}
	cfg, ok := s.profileConfig(p)
	if !ok {
		return cfg, false
	}
	s.cfgMu.Lock()
	s.llmProf = p.Name
	s.cfgMu.Unlock()
	return cfg, true
}

// errLegacyOAuthProfile 은 레거시 설정 저장이 구독 인증의 "default" 프로필을 덮으려 했다는 뜻이다.
var errLegacyOAuthProfile = errors.New(`legacy LLM config cannot overwrite the OAuth "default" profile`)

// legacyOAuthProfileMessage 는 errLegacyOAuthProfile을 받은 사용자에게 보일 문구다.
const legacyOAuthProfileMessage = "default 프로필이 구독 인증 프로필이라 이 설정으로 바꿀 수 없다. LLM 프로필 화면에서 바꿔야 한다"

// saveLLMConfig persists the LLM config as the active "default" profile in PG.
// "default"가 구독 OAuth 프로필이면 errLegacyOAuthProfile을 올린다. 이 경로는 API 키 설정만
// 다뤄서, 덮으면 OAuth 행에 API 키·중계 주소가 남는다.
func (s *Server) saveLLMConfig(cfg agent.Config) error {
	// cfg.Provider() already returns one of the three valid format strings
	// (anthropic / openai / openai-responses), matching the DB CHECK constraint.
	format := cfg.Provider()
	var id int64
	// 이 이전 버전 엔드포인트의 요청 본문에는 장애 조치·스트리밍·출력 최대 토큰 파라미터가
	// 없으므로 DB에 저장된 값을 그대로 다시 쓴다. 그러지 않으면 저장할 때마다 프로필의
	// priority, pool_exclude, streaming, 출력 최대 토큰(max_tokens / max_tokens_field)이
	// 소리 없이 영값으로 초기화된다.
	var priority int
	var poolExclude bool
	streaming := true // 이전 DB와 새 프로필 모두 기본값은 스트리밍
	var maxTokens int
	var maxTokensField string
	var sessionHeaderKey string
	var retry db.RetryOverride
	if profs, _ := s.m.pg.ListProfiles(); profs != nil {
		for _, p := range profs {
			if p.Name == "default" {
				if p.AuthType.IsOAuth() {
					return errLegacyOAuthProfile
				}
				id, priority, poolExclude, streaming = p.ID, p.Priority, p.PoolExclude, p.Streaming
				maxTokens, maxTokensField = p.MaxTokens, p.MaxTokensField
				sessionHeaderKey = p.SessionHeaderKey
				retry = p.Retry
				break
			}
		}
	}
	newID, err := s.m.pg.SaveProfile(&db.LLMProfile{
		ID: id, Name: "default", Format: format, Model: cfg.Model, BaseURL: cfg.BaseURL, Proxy: cfg.Proxy,
		APIKey: cfg.APIKey, RatePerSecond: cfg.RatePerSecond, RatePerMinute: cfg.RatePerMinute,
		ContextWindowK: cfg.ContextWindowK, ThinkingType: cfg.ThinkingType, ReasoningEffort: cfg.ReasoningEffort, IsDefault: true,
		Priority: priority, PoolExclude: poolExclude, Streaming: streaming,
		MaxTokens: maxTokens, MaxTokensField: maxTokensField, SessionHeaderKey: sessionHeaderKey,
		Retry: retry,
	})
	if err != nil {
		return err
	}
	return s.m.pg.SetActiveProfile(newID)
}

// reapplyActiveProfile hot-reloads the engine from the active DB profile so that
// saving or activating a profile takes effect without a restart. Best-effort:
// logs on failure and leaves the running engine untouched.
func (s *Server) reapplyActiveProfile() {
	cfg, ok := s.loadLLMConfig()
	if !ok {
		return
	}
	if err := s.applyLLM(cfg); err != nil {
		log.Printf("[engine] reapply active profile failed: %v", err)
		return
	}
	log.Printf("[engine] LLM reapplied from active profile: %s / %s", cfg.Provider(), cfg.Model)
}

// webSearchFor gates the global web-search opts (backend/key) by an agent's own
// web_search flag: the backend/key come from the global config, each agent decides on/off.
func (s *Server) webSearchFor(key string) agent.WebSearchOpts {
	o := s.m.WebSearchOpts()
	if a, err := s.m.pg.GetAgentByKey(key); err != nil || a == nil || !a.WebSearch {
		o.Enabled = false
	}
	return o
}

// nonStreamingResolver returns a resolver capturing a profile's streaming choice.
// The agents read it per run; a profile change rebuilds the agents (applyLLM),
// so the captured value is always the one in effect for this build.
func nonStreamingResolver(cfg agent.Config) func() bool {
	nonStreaming := !cfg.Stream
	return func() bool { return nonStreaming }
}

// maxTokensResolver mirrors nonStreamingResolver for the per-reply output cap.
func maxTokensResolver(cfg agent.Config) func() int {
	maxTokens := cfg.MaxTokens
	return func() int { return maxTokens }
}

// applyLLM (re)builds the planner/worker/main-agent from cfg and installs them on
// the running engine as the GLOBAL active pair. Safe to call at runtime (UI configures LLM).
func (s *Server) applyLLM(cfg agent.Config) error {
	var prov llm.Provider
	// A persisted active profile must use the same cached provider as task chains
	// and Agent bindings, otherwise each path owns a separate rate limiter.
	if active, _ := s.m.pg.ActiveProfile(); active != nil {
		if activeCfg, ok := s.loadProfileConfig(active.ID); ok && activeCfg == cfg {
			prov, _, _ = s.providerForProfile(active.ID)
		}
	}
	if prov == nil {
		var err error
		prov, err = cfg.NewProvider()
		if err != nil {
			return err
		}
		// Non-persisted/env configs do not have a profile cache key.
		s.cfgMu.Lock()
		profName := s.llmProf
		s.cfgMu.Unlock()
		prov = llmrec.Wrap(prov, s.m.PG(), cfg.Model, profName, cfg.ThinkingType, cfg.ReasoningEffort, s.m.LLMRecordEnabled)
		prov = bindSideProvider(prov, cfg, 0, profName)
	}
	s.cfgMu.Lock()
	s.llmDirect = prov
	s.cfgMu.Unlock()
	// LLM 장애 조치(기본값 꺼짐): 활성 프로필을 장애 조치 체인으로 감싸 현재 프로필을 쓸 수
	// 없으면 다음 프로필로 자동 전환한다. '전역 활성 프로필을 쓰는' 이 경로에만 영향을 준다.
	// 에이전트에 연결했거나 작업에 고정한 프로필은 providerForProfile을 거치고 기본적으로
	// 그 프로필만 쓴다(poolForBinding 참고). 꺼져 있거나 예비 프로필이 없으면 원래 provider를
	// 돌려주므로 동작이 바뀌지 않는다.
	if act, err := s.m.pg.ActiveProfile(); err == nil && act != nil {
		prov = s.poolForActive(act.ID, prov, cfg)
	}
	// No global planner/worker pair: every task runs on its own task-routed pair
	// (agentsForTask), resolved through the engine's authoritative resolver. Global
	// readiness (Ready()/llm_configured) is reported from s.llmOn, set below.

	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	win := cfg.CompactionWindow()
	// chat stays the GLOBAL fallback since one ChatAgent serves many agent keys — its
	// per-agent binding is resolved at Chat time (runConversationSync →
	// chatAgentForProfile). The main-agent has no global instance: each task builds its
	// own via agentsForTask (task-routed), so nothing is constructed for it here.
	s.cfgMu.Lock()
	// 대화 에이전트는 key로 여러 사용자 지정 에이전트를 맡으므로 전역 옵션(backend/key)을
	// 들고, Chat 시점에 대화 에이전트별로 Enabled를 판단한다. 대화는 늘 활성 프로필을 쓴다.
	s.chatAgent = agent.NewChatAgent(prov, cfg.Model, s.m.dir, tx, win) // chat page runner
	s.chatAgent.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	s.chatAgent.SetWebSearch(s.m.WebSearchOpts())
	s.chatAgent.SetGuard(s.chatGuard())
	s.chatAgent.SetNonStreaming(nonStreamingResolver(cfg))
	s.chatAgent.SetMaxTokens(maxTokensResolver(cfg))
	s.chatAgent.SetNoaEnabled(s.m.NoaCompactionEnabled) // 실험 기능: noa 컨텍스트 압축(run마다 읽는다)
	s.llmProv = prov
	s.llmCfg = cfg
	s.llmOn = true
	s.cfgMu.Unlock()
	s.invalidateTaskAgents()

	// wake the active task so a task created while idle starts exploring.
	if t := s.m.ActiveTask(); t != nil {
		t.Notify()
	}
	return nil
}

// loadProfileConfig builds an agent.Config from a specific profile id (with its key).
// ok=false when the profile is missing or has no api key.
// 구독 OAuth 프로필은 API 키 대신 저장된 OAuth 자격 증명이 없으면 ok=false 다.
func (s *Server) loadProfileConfig(id int64) (agent.Config, bool) {
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || p == nil {
		return agent.Config{}, false
	}
	return s.profileConfig(p)
}

// oauthCheckTimeout 은 프로필 설정을 만들 때 자격 증명이 있는지 DB 에서 확인하는 상한이다.
const oauthCheckTimeout = 5 * time.Second

// profileConfig 는 저장된 프로필(키 포함)로 agent.Config를 만든다. 요청을 보낼 수 없는
// 프로필(API 키 없음, OAuth 자격 증명 없음)이면 ok=false 다.
func (s *Server) profileConfig(p *db.LLMProfile) (agent.Config, bool) {
	cfg := agent.ConfigFrom(profileFormat(p.AuthType, p.Format), p.Model, p.BaseURL, p.APIKey, p.Proxy)
	cfg.RatePerSecond, cfg.RatePerMinute = p.RatePerSecond, p.RatePerMinute
	cfg.ContextWindowK = p.ContextWindowK
	cfg.ThinkingType = p.ThinkingType
	cfg.ReasoningEffort = p.ReasoningEffort
	cfg.Stream = p.Streaming
	cfg.MaxTokens, cfg.MaxTokensField = p.MaxTokens, p.MaxTokensField
	cfg.SessionHeaderKey = p.SessionHeaderKey
	s.applyProfileRetry(&cfg, p)
	if !p.AuthType.IsOAuth() {
		return cfg, cfg.APIKey != ""
	}
	if p.AuthType == db.AuthClaudeOAuth {
		applyClaudeOAuthRules(&cfg)
		cfg.BaseURL = s.claudeURL()
	} else {
		applyChatGPTOAuthRules(&cfg, s.codexURL())
	}
	ctx, cancel := context.WithTimeout(s.ctxOrBackground(), oauthCheckTimeout)
	defer cancel()
	src, err := s.oauth.connectedSourceForAuth(ctx, p.ID, p.AuthType)
	if err != nil {
		if !errors.Is(err, errOAuthNotConnected) {
			log.Printf("[engine] LLM profile %d OAuth credentials unavailable: %v", p.ID, err)
		}
		return cfg, false
	}
	cfg.OAuthTokens = src
	return cfg, true
}

// profileFormat 은 프로필이 실제로 쓸 형식이다. 구독 인증은 저장값과 무관하게 제공자 형식을 쓴다.
// ConfigFrom이 형식에 맞춰 주소·기본 모델을 고르므로 그 전에 정한다.
func profileFormat(authType db.AuthType, format string) string {
	if authType == db.AuthClaudeOAuth {
		return string(llm.FormatAnthropic)
	}
	if authType == db.AuthChatGPTOAuth {
		return string(llm.FormatOpenAIResponses)
	}
	return format
}

// applyChatGPTOAuthRules 는 Codex 백엔드가 받는 형식으로 설정을 고정한다. Codex 백엔드는
// Responses 형식의 stream:true 요청만 받으므로 프로필에 저장된 형식·수신 방식과 무관하게 맞춘다.
// 주소는 codexBaseURL 만 쓴다. 저장되거나 요청에 온 base_url을 따르면 구독 토큰이 그 호스트
// (API 키용 중계 등)로 나간다. API 키는 쓰지 않으므로 비운다.
func applyChatGPTOAuthRules(cfg *agent.Config, codexBaseURL string) {
	cfg.AuthType = db.AuthChatGPTOAuth
	cfg.Format = llm.FormatOpenAIResponses
	cfg.Stream = true
	cfg.APIKey = ""
	cfg.BaseURL = codexBaseURL
}

// codexURL 은 chatgpt_oauth 프로필이 부를 Codex 백엔드 주소다.
func (s *Server) codexURL() string {
	if s.codexBaseURL != "" {
		return s.codexBaseURL
	}
	return agent.CodexBaseURL
}

// ctxOrBackground 는 서버 수명 ctx를 돌려준다. 테스트처럼 ctx 없이 만든 Server는 Background를 쓴다.
func (s *Server) ctxOrBackground() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// effectiveProfileForAgent resolves the LLM profile id an agent should run on, by
// precedence: agent binding (agents.llm_profile_id) → pin (task/conversation) → nil
// (caller falls back to the global active profile). A binding to a deleted profile
// can't happen (FK ON DELETE SET NULL); an otherwise-invalid one is dropped downstream
// by loadProfileConfig, letting the caller fall back.
func (s *Server) effectiveProfileForAgent(agentKey string, pinID *int64) *int64 {
	if s.m.pg != nil && agentKey != "" {
		if a, _ := s.m.pg.GetAgentByKey(agentKey); a != nil && a.LLMProfileID != nil {
			return a.LLMProfileID
		}
	}
	return pinID
}

// resolveChatAgent 는 대화 하나에 쓸 ChatAgent를 고른다. 에이전트 자체에 연결한 프로필이나
// 이 대화에서 고른 프로필을 먼저 쓰고, 전역 활성 프로필은 대체로만 쓴다. 보내기 전 검사와
// 백그라운드 실행기가 반드시 이 함수를 함께 써야 한다. 두 경로가 다르게 고르면, 올바른
// 프로필을 고른 대화도 전역 활성 프로필이 없을 때 'LLM 설정 안 됨'으로 거부된다.
func (s *Server) resolveChatAgent(c *db.Conversation) *agent.ChatAgent {
	ca := s.chatAgentRef()
	if eff := s.effectiveProfileForAgent(c.AgentKey, c.LLMProfileID); eff != nil {
		if pa := s.chatAgentForProfile(*eff); pa != nil {
			ca = pa
		}
	}
	return ca
}

// chatUnavailableReason explains why no ChatAgent could be resolved, so the user
// knows whether to add a config, activate one, or pick one for this conversation
// — rather than a flat "not configured" that hides which of those it is.
func (s *Server) chatUnavailableReason() string {
	if s.m.pg != nil {
		if profiles, err := s.m.pg.ListProfiles(); err == nil && len(profiles) == 0 {
			return "LLM 프로필이 없습니다. '시스템 → LLM'에서 프로필을 추가하세요"
		}
		if active, err := s.m.pg.ActiveProfile(); err == nil && active == nil {
			return "활성 LLM 프로필이 없습니다. '시스템 → LLM'에서 프로필 하나를 활성으로 설정하거나, 이 대화에 쓸 프로필을 지정하세요"
		}
	}
	return "LLM이 준비되지 않아 대화할 수 없습니다. '시스템 → LLM'에 사용 가능한 활성 프로필이 있는지 확인하세요"
}

// providerForProfile returns a cached provider+cfg for a profile id, so every agent
// bound/pinned to the same profile shares one provider instance (one rate limiter).
// ok=false when the profile is missing/invalid → caller falls back to the global pair.
func (s *Server) providerForProfile(id int64) (llm.Provider, agent.Config, bool) {
	s.provCacheMu.Lock()
	if e := s.provByProfile[id]; e != nil {
		s.provCacheMu.Unlock()
		return e.prov, e.cfg, true
	}
	generation := s.provCacheGen
	s.provCacheMu.Unlock()
	cfg, ok := s.loadProfileConfig(id)
	if !ok {
		return nil, agent.Config{}, false
	}
	prov, err := cfg.NewProvider()
	if err != nil {
		log.Printf("[engine] build provider for LLM profile %d failed: %v", id, err)
		return nil, agent.Config{}, false
	}
	// Wrap with recorder, tagged with this profile's name.
	if p, _ := s.m.pg.ProfileByID(id); p != nil {
		prov = llmrec.Wrap(prov, s.m.PG(), cfg.Model, p.Name, cfg.ThinkingType, cfg.ReasoningEffort, s.m.LLMRecordEnabled)
		prov = bindSideProvider(prov, cfg, id, p.Name)
	}
	s.provCacheMu.Lock()
	if generation != s.provCacheGen {
		s.provCacheMu.Unlock()
		return s.providerForProfile(id)
	}
	if e := s.provByProfile[id]; e != nil { // lost the race → keep the winner
		prov, cfg = e.prov, e.cfg
	} else {
		s.provByProfile[id] = &provEntry{prov: prov, cfg: cfg}
		log.Printf("[engine] built provider for LLM profile %d (%s / %s)", id, cfg.Provider(), cfg.Model)
	}
	s.provCacheMu.Unlock()
	return prov, cfg, true
}

// chatAgentForProfile returns a ChatAgent built from a specific LLM profile, cached
// per profile id. Returns nil if the profile is missing or has no API key.
func (s *Server) chatAgentForProfile(id int64) *agent.ChatAgent {
	s.profMu.Lock()
	cached := s.profChatAgents[id]
	s.profMu.Unlock()
	if cached != nil {
		return cached
	}
	prov, cfg, ok := s.providerForProfile(id)
	if !ok {
		return nil
	}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	ca := agent.NewChatAgent(s.poolForBinding(id, prov, cfg), cfg.Model, s.m.dir, tx, cfg.CompactionWindow())
	ca.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	ca.SetWebSearch(s.m.WebSearchOpts())
	ca.SetGuard(s.chatGuard())
	ca.SetNonStreaming(nonStreamingResolver(cfg))
	ca.SetMaxTokens(maxTokensResolver(cfg))
	ca.SetNoaEnabled(s.m.NoaCompactionEnabled) // 실험 기능: noa 컨텍스트 압축(run마다 읽는다)
	s.profMu.Lock()
	if ex := s.profChatAgents[id]; ex != nil { // lost the race → keep the winner
		ca = ex
	} else {
		s.profChatAgents[id] = ca
	}
	s.profMu.Unlock()
	return ca
}

// invalidateProfileAgents drops the per-profile ChatAgent + provider caches so a profile
// save/activate/delete — or an agent's binding change — rebuilds pinned tasks' agents
// (and re-resolves each agent's bound model) on their next round.
func (s *Server) invalidateProfileAgents() {
	s.profMu.Lock()
	s.profChatAgents = map[int64]*agent.ChatAgent{}
	s.profMu.Unlock()
	s.provCacheMu.Lock()
	s.provCacheGen++
	s.provByProfile = map[int64]*provEntry{}
	s.provCacheMu.Unlock()
	s.invalidateTaskAgents()
}

func (s *Server) chatAgentRef() *agent.ChatAgent {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.chatAgent
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerSideRoutes(mux)

	// Auth routes — exempt from JWT check (handled in requireAuth)
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/init", s.authInit)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/change-password", s.authChangePassword)

	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/logs", s.getLogs)
	mux.HandleFunc("GET /api/logs/history", s.getLogsHistory)
	mux.HandleFunc("GET /api/logs/stream", s.streamLogs)

	// 화면의 원클릭 업데이트. 기본 JWT 인증을 거치므로(auth.go는 /api/auth/*와
	// /api/health만 통과시킨다) 프로그램 자체를 바꾸는 이 API들은 당연히 로그인이 필요하다.
	mux.HandleFunc("GET /api/update/check", s.updateCheck)
	mux.HandleFunc("POST /api/update/apply", s.updateApply)
	mux.HandleFunc("POST /api/update/rollback", s.updateRollback)
	mux.HandleFunc("GET /api/update/stream", s.updateStream)

	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("GET /api/task-categories", s.pgListTaskCategories)
	mux.HandleFunc("POST /api/task-categories", s.pgCreateTaskCategory)
	mux.HandleFunc("PATCH /api/task-categories/{id}", s.pgRenameTaskCategory)
	mux.HandleFunc("DELETE /api/task-categories/{id}", s.pgDeleteTaskCategory)
	mux.HandleFunc("POST /api/tasks/category/batch", s.updateTasksCategoryBatch)
	mux.HandleFunc("GET /api/task-templates", s.pgListTaskTemplates)
	mux.HandleFunc("POST /api/task-templates", s.pgCreateTaskTemplate)
	mux.HandleFunc("PATCH /api/task-templates/{id}", s.pgUpdateTaskTemplate)
	mux.HandleFunc("DELETE /api/task-templates/{id}", s.pgDeleteTaskTemplate)
	mux.HandleFunc("GET /api/tasks/{id}", s.getTask)
	mux.HandleFunc("PATCH /api/tasks/{id}", s.updateTaskMetadata)
	mux.HandleFunc("PATCH /api/tasks/{id}/category", s.updateTaskCategory)
	// 작업 단위 자산 차단·허용 규칙
	mux.HandleFunc("GET /api/tasks/{id}/intercept-rules", s.taskInterceptListRules)
	mux.HandleFunc("POST /api/tasks/{id}/intercept-rules", s.taskInterceptCreateRule)
	mux.HandleFunc("PUT /api/tasks/{id}/intercept-rules/{rid}", s.taskInterceptUpdateRule)
	mux.HandleFunc("DELETE /api/tasks/{id}/intercept-rules/{rid}", s.taskInterceptDeleteRule)
	mux.HandleFunc("POST /api/tasks/{id}/intercept-rules/{rid}/toggle", s.taskInterceptToggleRule)
	mux.HandleFunc("POST /api/tasks/control/batch", s.controlTasksBatch)
	mux.HandleFunc("GET /api/task-archives", s.listTaskArchives)
	mux.HandleFunc("GET /api/task-archives/{id}", s.getTaskArchive)
	mux.HandleFunc("POST /api/tasks/{id}/archive", s.queueTaskArchive)
	mux.HandleFunc("POST /api/tasks/archive/batch", s.queueTaskArchivesBatch)
	mux.HandleFunc("POST /api/task-archives/{id}/restore", s.queueTaskArchiveRestore)
	mux.HandleFunc("POST /api/task-archives/restore/batch", s.restoreTaskArchivesBatch)
	mux.HandleFunc("DELETE /api/task-archives/{id}", s.queueTaskArchiveDelete)
	mux.HandleFunc("POST /api/task-archives/delete/batch", s.deleteTaskArchivesBatch)
	mux.HandleFunc("GET /api/tasks/{id}/coverage", s.taskCoverage)
	mux.HandleFunc("GET /api/tasks/{id}/coverage-graph", s.taskCoverageGraph)
	mux.HandleFunc("GET /api/tasks/{id}/asset-refs", s.taskAssetRefs)
	mux.HandleFunc("POST /api/tasks/{id}/assets", s.attachTaskAssets)
	mux.HandleFunc("DELETE /api/tasks/{id}/assets/{assetID}", s.detachTaskAsset)
	mux.HandleFunc("GET /api/tasks/{id}/intent-assets", s.taskIntentAssets)

	// 작업 공간 파일 관리자(workDir 대상)
	mux.HandleFunc("GET /api/workspace/list", s.wsList)
	mux.HandleFunc("GET /api/workspace/read", s.wsRead)
	mux.HandleFunc("POST /api/workspace/write", s.wsWrite)
	mux.HandleFunc("POST /api/workspace/mkdir", s.wsMkdir)
	mux.HandleFunc("DELETE /api/workspace/delete", s.wsDelete)
	mux.HandleFunc("GET /api/workspace/download", s.wsDownload)
	mux.HandleFunc("POST /api/workspace/upload", s.wsUpload)
	mux.HandleFunc("GET /api/tasks/{id}/scope", s.taskScopeList)
	mux.HandleFunc("POST /api/tasks/{id}/scope", s.taskScopeAdd)
	mux.HandleFunc("DELETE /api/tasks/{id}/scope/{sid}", s.taskScopeDelete)
	mux.HandleFunc("GET /api/tasks/{id}/goals", s.listGoals)                       // 목표 관리: 이 작업의 모든 목표 목록
	mux.HandleFunc("POST /api/tasks/{id}/goals", s.addGoal)                        // 목표 관리: 사람이 목표 추가(작업 재개)
	mux.HandleFunc("PATCH /api/tasks/{id}/goals/{gid}", s.editGoal)                // 목표 관리: 목표 수정(작업 재개)
	mux.HandleFunc("DELETE /api/tasks/{id}/goals/{gid}", s.deleteGoal)             // 목표 관리: 목표 영구 삭제(재개하지 않음)
	mux.HandleFunc("GET /api/tasks/{id}/constraints", s.listConstraints)           // 제약 조건 관리: 이 작업의 작업 제약 조건 목록
	mux.HandleFunc("POST /api/tasks/{id}/constraints", s.addConstraint)            // 제약 조건 관리: 제약 조건 추가(planner에 알리지 않음)
	mux.HandleFunc("PATCH /api/tasks/{id}/constraints/{cid}", s.editConstraint)    // 제약 조건 관리: 제약 조건 수정
	mux.HandleFunc("DELETE /api/tasks/{id}/constraints/{cid}", s.deleteConstraint) // 제약 조건 관리: 제약 조건 삭제
	mux.HandleFunc("POST /api/tasks/{id}/control", s.control)
	mux.HandleFunc("PUT /api/tasks/{id}/llm", s.updateTaskLLMProfiles)
	mux.HandleFunc("GET /api/tasks/{id}/llm/resolution", s.taskLLMResolutionHandler)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/control", s.controlIntent)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/messages", s.sendWorkerMessage)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/rerun", s.rerunIntent)    // blocked/exhausted/stopped 의도 한 건 재실행
	mux.HandleFunc("POST /api/tasks/{id}/intents/rerun-blocked", s.rerunBlocked) // 이 작업의 blocked 의도 전체 일괄 재실행
	mux.HandleFunc("POST /api/active", s.setActive)

	mux.HandleFunc("GET /api/llm", s.getLLM)
	mux.HandleFunc("POST /api/llm", s.setLLM)
	mux.HandleFunc("POST /api/llm/test", s.testLLM)

	// asset system
	mux.HandleFunc("GET /api/assets", s.listAssets)
	mux.HandleFunc("GET /api/assets/counts", s.assetCounts)
	mux.HandleFunc("POST /api/assets", s.insertAssets)
	mux.HandleFunc("DELETE /api/assets", s.deleteAssets)

	// company system
	mux.HandleFunc("GET /api/companies", s.listCompanies)
	mux.HandleFunc("POST /api/companies", s.createCompany)
	mux.HandleFunc("GET /api/companies/{id}", s.getCompany)
	mux.HandleFunc("DELETE /api/companies/{id}", s.deleteCompany)
	mux.HandleFunc("POST /api/companies/{id}/scope", s.addCompanyScope)
	mux.HandleFunc("POST /api/companies/reattribute", s.reattribute)

	mux.HandleFunc("GET /api/exploration/frontier", s.frontier)
	mux.HandleFunc("GET /api/exploration/findings", s.findings)
	mux.HandleFunc("GET /api/exploration/findings/groups", s.findingGroups)
	mux.HandleFunc("GET /api/exploration/findings/asset-tree", s.findingAssetTree)
	mux.HandleFunc("GET /api/exploration/findings/stats", s.findingStats)
	mux.HandleFunc("GET /api/exploration/findings/export", s.findingsExport)
	s.registerFindingTraffic(mux)
	mux.HandleFunc("GET /api/exploration/findings/{id}", s.getFinding)
	mux.HandleFunc("GET /api/exploration/findings/{id}/lineage", s.findingLineage)
	mux.HandleFunc("POST /api/exploration/findings/{id}/deepen", s.deepenFinding)
	mux.HandleFunc("GET /api/exploration/findings/{id}/retests", s.listFindingRetests)
	mux.HandleFunc("GET /api/exploration/findings/retests/active", s.listActiveFindingRetests)
	mux.HandleFunc("POST /api/exploration/findings/{id}/retests", s.startFindingRetest)
	mux.HandleFunc("PATCH /api/exploration/findings/{id}", s.patchFinding)
	mux.HandleFunc("DELETE /api/exploration/findings/{id}", s.deleteFinding)
	mux.HandleFunc("GET /api/exploration/intents", s.intents)
	mux.HandleFunc("GET /api/exploration/graph", s.explorationGraph)
	mux.HandleFunc("GET /api/exploration/nodes", s.explorationNodes)
	mux.HandleFunc("GET /api/exploration/activity", s.activity)
	mux.HandleFunc("GET /api/exploration/activity/history", s.activityHistory)
	mux.HandleFunc("GET /api/exploration/main-sessions", s.mainSessions)
	mux.HandleFunc("POST /api/exploration/main-session/new", s.newMainSession)
	mux.HandleFunc("GET /api/exploration/activity/stream", s.streamActivity)
	mux.HandleFunc("GET /api/exploration/activity/{seq}", s.activityDetail)
	mux.HandleFunc("GET /api/exploration/tokens", s.tokenStats)
	mux.HandleFunc("GET /api/tokens/daily", s.tokenDailyStats)
	mux.HandleFunc("GET /api/tokens/conversations", s.conversationTokens)
	mux.HandleFunc("GET /api/tokens/usage", s.pgUsageStats) // 전역 llm_usage 집계(대시보드 새 보기)

	mux.HandleFunc("GET /api/audit", s.getAudit)
	mux.HandleFunc("POST /api/gc", s.gc)
	mux.HandleFunc("GET /api/traffic", s.getTraffic)
	mux.HandleFunc("GET /api/traffic/hosts", s.getTrafficHosts)
	mux.HandleFunc("DELETE /api/traffic", s.deleteTraffic)
	mux.HandleFunc("DELETE /api/traffic/hosts", s.deleteTrafficHosts)
	mux.HandleFunc("DELETE /api/traffic/all", s.deleteAllTraffic)
	mux.HandleFunc("GET /api/traffic/exchange", s.getTrafficExchange)
	mux.HandleFunc("GET /api/traffic/blob", s.getTrafficBlob)
	mux.HandleFunc("GET /api/commands", s.pgListCommands)
	mux.HandleFunc("GET /api/commands/stats", s.pgToolStats) // 도구별 호출 횟수 집계
	mux.HandleFunc("GET /api/llm/records", s.pgListLLMRecords)
	mux.HandleFunc("DELETE /api/llm/records", s.pgDeleteLLMRecords)
	mux.HandleFunc("GET /api/llm/records/tasks", s.pgLLMTasks)
	mux.HandleFunc("GET /api/llm/records/by-model", s.pgTokenByModel) // 이 작업의 모델별 토큰 사용량 집계
	mux.HandleFunc("GET /api/llm/records/{id}", s.pgGetLLMRecord)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	// 취약점 IM 알림. 알림 채널은 '여러 개 + 각자의 필터 규칙'을 가진 자원이라 평평한
	// /api/settings 키·값에 넣지 않고 별도 REST API 묶음으로 둔다.
	mux.HandleFunc("GET /api/notify/meta", s.notifyMeta)
	mux.HandleFunc("GET /api/notify/channels", s.notifyListChannels)
	mux.HandleFunc("POST /api/notify/channels", s.notifyCreateChannel)
	mux.HandleFunc("PATCH /api/notify/channels/{id}", s.notifyUpdateChannel)
	mux.HandleFunc("DELETE /api/notify/channels/{id}", s.notifyDeleteChannel)
	mux.HandleFunc("POST /api/notify/channels/{id}/test", s.notifyTestChannel)
	mux.HandleFunc("GET /api/notify/deliveries", s.notifyListDeliveries)
	mux.HandleFunc("POST /api/notify/deliveries/{id}/retry", s.notifyRetryDelivery)
	mux.HandleFunc("POST /api/settings/web-search/test", s.testWebSearch)
	mux.HandleFunc("GET /api/report", s.getReport)
	mux.HandleFunc("GET /api/chat/mentions", s.searchChatMentions)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/chat/upload", s.chatUpload) // 방식 1 파일 업로드: 세션·작업 작업 디렉터리의 uploads/에 저장한다
	mux.HandleFunc("GET /api/tasks/{id}/chat/status", s.taskChatStatus)
	mux.HandleFunc("POST /api/tasks/{id}/chat/stop", s.stopChat)

	// --- 관리 API(PostgreSQL 데이터 소스) ---
	mux.HandleFunc("DELETE /api/tasks/{id}", s.pgDeleteTask)
	// Agents
	// conversations (chat page)
	mux.HandleFunc("GET /api/conversations", s.pgListConversations)
	mux.HandleFunc("POST /api/conversations", s.pgCreateConversation)
	mux.HandleFunc("POST /api/conversations/delete/batch", s.pgDeleteConversationsBatch)
	mux.HandleFunc("PATCH /api/conversations/{id}", s.pgRenameConversation)
	mux.HandleFunc("PATCH /api/conversations/{id}/profile", s.pgUpdateConversation)
	mux.HandleFunc("DELETE /api/conversations/{id}", s.pgDeleteConversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages", s.pgConversationMessages)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.pgSendConversationMessage)
	mux.HandleFunc("POST /api/conversations/{id}/stop", s.pgStopConversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages/{seq}", s.pgConversationMsgDetail)

	mux.HandleFunc("GET /api/agents", s.pgListAgents)
	mux.HandleFunc("POST /api/agents", s.pgCreateAgent)
	mux.HandleFunc("GET /api/agents/{key}", s.pgGetAgent)
	mux.HandleFunc("PATCH /api/agents/{key}", s.pgUpdateAgent)
	mux.HandleFunc("DELETE /api/agents/{key}", s.pgDeleteAgent)
	mux.HandleFunc("PUT /api/agents/{key}/config", s.pgSaveAgentConfig)
	mux.HandleFunc("PUT /api/agents/{key}/prompt", s.pgSavePrompt)
	mux.HandleFunc("POST /api/agents/{key}/prompt/reset", s.pgResetPrompt)
	mux.HandleFunc("PUT /api/agents/{key}/wrapup", s.pgSaveWrapup)
	mux.HandleFunc("POST /api/agents/{key}/wrapup/reset", s.pgResetWrapup)
	mux.HandleFunc("PUT /api/agents/{key}/wrapup/task-timeout", s.pgSaveTaskTimeoutWrapup)
	mux.HandleFunc("POST /api/agents/{key}/wrapup/task-timeout/reset", s.pgResetTaskTimeoutWrapup)
	mux.HandleFunc("GET /api/agents/{key}/triggers", s.pgListTriggers)
	mux.HandleFunc("POST /api/agents/{key}/triggers", s.pgCreateTrigger)
	mux.HandleFunc("PATCH /api/triggers/{id}", s.pgUpdateTrigger)
	mux.HandleFunc("DELETE /api/triggers/{id}", s.pgDeleteTrigger)
	mux.HandleFunc("GET /api/agents/{key}/prompts", s.pgListPromptVersions)
	mux.HandleFunc("GET /api/agents/{key}/variables", s.pgPromptVars)
	mux.HandleFunc("POST /api/agents/{key}/prompt/preview", s.pgPreviewPrompt)
	mux.HandleFunc("GET /api/agents/{key}/visibility", s.pgGetAgentVisibility)
	mux.HandleFunc("PUT /api/agents/{key}/visibility", s.pgSetAgentVisibility)
	// 내장 도구 목록(설명·파라미터 기본값을 고칠 수 있고 에이전트별로 연결한다. key와 handler는 코드에 있다)
	mux.HandleFunc("GET /api/tools", s.pgListTools)
	mux.HandleFunc("PUT /api/tools/{key}", s.pgUpdateTool)
	mux.HandleFunc("POST /api/tools/custom", s.pgCreateCustomTool)
	mux.HandleFunc("POST /api/tools/custom/test", s.pgTestCustomTool)
	mux.HandleFunc("PUT /api/tools/custom/{key}", s.pgUpdateCustomTool)
	mux.HandleFunc("DELETE /api/tools/custom/{key}", s.pgDeleteCustomTool)
	mux.HandleFunc("POST /api/settings/python/detect", s.pgDetectPython)
	mux.HandleFunc("POST /api/tools/{key}/reset", s.pgResetTool)
	// MCP CRUD
	mux.HandleFunc("GET /api/mcp", s.pgListMCP)
	mux.HandleFunc("POST /api/mcp", s.pgSaveMCP)
	mux.HandleFunc("DELETE /api/mcp/{id}", s.pgDeleteMCP)
	mux.HandleFunc("GET /api/mcp/{id}/tools", s.pgMCPTools)
	mux.HandleFunc("POST /api/mcp/{id}/refresh", s.pgRefreshMCP)
	// 자산 동기화 — ScopeSentry 데이터 소스
	mux.HandleFunc("GET /api/sync/scopesentry/status", s.syncSSStatus)
	mux.HandleFunc("POST /api/sync/scopesentry/datasource", s.syncSSDatasource)
	mux.HandleFunc("GET /api/sync/scopesentry/projects", s.syncSSProjects)
	mux.HandleFunc("GET /api/sync/scopesentry/tasks", s.syncSSTasks)
	mux.HandleFunc("POST /api/sync/scopesentry/sync", s.syncSSRun)
	// 스킬 CRUD(파일 시스템)
	mux.HandleFunc("GET /api/skills", s.fsListSkills)
	mux.HandleFunc("POST /api/skills", s.fsCreateSkill)
	mux.HandleFunc("POST /api/skills/upload", s.fsUploadSkill)
	mux.HandleFunc("DELETE /api/skills/{name}", s.fsDeleteSkill)
	mux.HandleFunc("GET /api/skills/missing", s.fsMissingSkills)   // 없는 스킬 호출(호출하려 했지만 없는 스킬 이름)
	mux.HandleFunc("GET /api/skills/{name}/usage", s.fsSkillUsage) // 스킬 하나의 최근 호출
	mux.HandleFunc("PUT /api/skills/{name}/meta", s.fsUpdateSkillMeta)
	mux.HandleFunc("POST /api/skills/{name}/dirs", s.fsCreateDir)
	mux.HandleFunc("GET /api/skills/{name}/files", s.fsListFiles)
	// {file...} captures path segments including slashes (e.g. scripts/extract.py)
	mux.HandleFunc("GET /api/skills/{name}/files/{file...}", s.fsReadFile)
	mux.HandleFunc("PUT /api/skills/{name}/files/{file...}", s.fsWriteFile)
	mux.HandleFunc("DELETE /api/skills/{name}/files/{file...}", s.fsDeletePath)
	// MCP 자원 쪽 공개 범위(더 구체적인 스킬 경로가 먼저 일치한다)
	mux.HandleFunc("GET /api/visibility/{kind}/{id}", s.pgResourceVisibility)
	mux.HandleFunc("POST /api/visibility/toggle", s.pgToggleVisibility)
	// 스킬 공개 범위(이름 기준. 더 구체적이라 위의 와일드카드 경로보다 먼저 일치한다)
	mux.HandleFunc("GET /api/visibility/skill/{name}", s.pgSkillVisibility)
	mux.HandleFunc("POST /api/visibility/skill/toggle", s.pgToggleSkillVisibility)
	// LLM 프로필 여러 개
	mux.HandleFunc("GET /api/llm/profiles", s.pgListProfiles)
	mux.HandleFunc("POST /api/llm/profiles", s.pgSaveProfile)
	mux.HandleFunc("DELETE /api/llm/profiles/{id}", s.pgDeleteProfile)
	mux.HandleFunc("POST /api/llm/profiles/active", s.pgActivateProfile)
	mux.HandleFunc("POST /api/llm/oauth/chatgpt/start", s.startChatGPTLogin)
	mux.HandleFunc("POST /api/llm/oauth/chatgpt/complete", s.completeChatGPTLogin)
	mux.HandleFunc("POST /api/llm/oauth/chatgpt/device", s.startChatGPTDeviceLogin)
	mux.HandleFunc("GET /api/llm/oauth/chatgpt/device/{id}", s.chatGPTDeviceLoginStatus)
	mux.HandleFunc("POST /api/llm/oauth/chatgpt/disconnect", s.disconnectChatGPT)
	mux.HandleFunc("POST /api/llm/oauth/claude/start", s.startClaudeLogin)
	mux.HandleFunc("POST /api/llm/oauth/claude/complete", s.completeClaudeLogin)
	mux.HandleFunc("POST /api/llm/oauth/claude/disconnect", s.disconnectClaude)
	mux.HandleFunc("GET /api/llm/oauth/claude/status/{id}", s.claudeLoginStatus)
	mux.HandleFunc("GET /api/llm/retry-policy", s.pgGetLLMRetryPolicy)
	mux.HandleFunc("POST /api/llm/retry-policy", s.pgSaveLLMRetryPolicy)
	mux.HandleFunc("GET /api/llm/pool", s.pgLLMPoolStatus)
	mux.HandleFunc("POST /api/llm/pool/reset", s.pgLLMPoolReset)
	mux.HandleFunc("POST /api/llm/models", s.pgListModels)

	// 차단 규칙 관리
	mux.HandleFunc("GET /api/intercept/rules", s.interceptListRules)
	mux.HandleFunc("POST /api/intercept/rules", s.interceptCreateRule)
	mux.HandleFunc("PUT /api/intercept/rules/{id}", s.interceptUpdateRule)
	mux.HandleFunc("DELETE /api/intercept/rules/{id}", s.interceptDeleteRule)
	mux.HandleFunc("POST /api/intercept/rules/{id}/toggle", s.interceptToggleRule)

	// 자산 차단 규칙 관리(전역 차단 목록: 도메인·IP·URL·CIDR)
	mux.HandleFunc("GET /api/asset-intercept/rules", s.assetInterceptListRules)
	mux.HandleFunc("POST /api/asset-intercept/rules", s.assetInterceptCreateRule)
	mux.HandleFunc("PUT /api/asset-intercept/rules/{id}", s.assetInterceptUpdateRule)
	mux.HandleFunc("DELETE /api/asset-intercept/rules/{id}", s.assetInterceptDeleteRule)
	mux.HandleFunc("POST /api/asset-intercept/rules/{id}/toggle", s.assetInterceptToggleRule)

	mux.HandleFunc("GET /api/intercept/pending", s.interceptListPending)
	mux.HandleFunc("GET /api/intercept/pending/{id}", s.interceptGetOne)
	mux.HandleFunc("POST /api/intercept/pending/{id}/decide", s.interceptDecide)
	mux.HandleFunc("GET /api/intercept/history", s.interceptHistory)
	mux.HandleFunc("GET /api/intercept/history/{id}", s.interceptDetail)
	mux.HandleFunc("GET /api/intercept/history/{id}/execution", s.interceptExecution)
	mux.HandleFunc("GET /api/intercept/task/{taskID}", s.interceptListTaskItems)
	mux.HandleFunc("GET /api/intercept/tool-config", s.interceptGetToolConfig)
	mux.HandleFunc("PUT /api/intercept/tool-config", s.interceptSetToolConfig)
	mux.HandleFunc("GET /api/intercept/judge", s.interceptGetJudgeConfig)
	mux.HandleFunc("PUT /api/intercept/judge", s.interceptSetJudgeConfig)
	mux.HandleFunc("GET /api/intercept/judge/usage", s.interceptJudgeUsage) // 모델 승인 심사 누적 토큰 사용량

	// /api/* goes through CORS + JWT; everything else is served by the embedded
	// frontend (public — auth is enforced client-side and on the API). With the
	// no-embed build the webui handler just 404s (run `next dev` separately).
	api := cors(s.requireAuth(mux))
	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/", s.webuiHandler())
	return root
}

// --- handlers ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "service": "artex", "version": BuildVersion})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	out["engine_mode"] = "idle"              // spec enum; overridden below when a task is active
	out["llm_configured"] = s.engine.Ready() // is an LLM provider installed at all
	if tr := s.m.Traffic(); tr != nil {
		c, _ := tr.Count()
		out["traffic"] = c
		out["traffic_enabled"] = true
	}
	if counts, err := s.m.Assets().CountsByType(); err == nil {
		total := 0
		for _, n := range counts {
			total += n
		}
		out["assets"] = total
		out["asset_counts"] = counts
	}

	// resolve the task: explicit ?task=<id> binds to that task (so a detail view
	// never silently follows a globally-changed active task); empty = active.
	taskParam := r.URL.Query().Get("task")
	t := s.m.ResolveTask(taskParam)
	if t == nil {
		if taskParam != "" && taskParam != "active" {
			writeErr(w, 404, "task not found")
			return
		}
		writeJSON(w, 200, out) // no task selected: only global fields
		return
	}
	out["llm_configured"] = s.engine.ReadyFor(t)

	st, _ := t.Store.Stats()
	out["exploration"] = st

	// per-task running state + heartbeat (distinct from "LLM configured").
	intents, _ := t.Store.ListByKind(db.KindIntent, 100000)
	inFlight := 0
	for _, in := range intents {
		if in.State == "running" {
			inFlight++
		}
	}
	goals, _ := t.Store.ListByKind(db.KindGoal, 10000)
	goalsMet := 0
	for _, g := range goals {
		if g.State == "met" {
			goalsMet++
		}
	}
	last := s.engine.LastActivity(t.ID)
	paused := s.engine.IsPaused(t.ID)
	activeCalls := s.engine.ActiveLLMCalls(t.ID)
	running := s.engine.ReadyFor(t) && s.engine.Started(t.ID) && !paused
	// A live event-driven engine with no work is healthy idle. Only a persisted
	// running intent without any corresponding LLM call can be considered stalled.
	stalled := running && activeCalls == 0 && inFlight > 0 && last > 0 && time.Now().Unix()-last > 120

	// engine_mode follows the spec enum (exploring|paused|stalled|idle).
	engineMode := "idle"
	switch {
	case paused:
		engineMode = "paused"
	case stalled:
		engineMode = "stalled"
	case running && activeCalls > 0:
		engineMode = "exploring"
	}
	out["engine_mode"] = engineMode

	out["active_task"] = map[string]any{
		"id": t.ID, "description": t.Description, "goal": t.Goal,
		"running":       running,
		"paused":        paused,
		"engine_mode":   engineMode,
		"in_flight":     inFlight,
		"llm_in_flight": activeCalls,
		"last_activity": last,
		"stalled":       stalled,
		"goals_total":   len(goals),
		"goals_met":     goalsMet,
	}
	writeJSON(w, 200, out)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	active := ""
	if t := s.m.ActiveTask(); t != nil {
		active = t.ID
	}
	list := s.m.List()
	metrics, _ := s.m.PG().TaskListMetricsAll()
	archiveBlockers, _ := s.m.PG().TaskArchiveBlockers()
	dtos := make([]TaskDTO, 0, len(list))
	for _, t := range list {
		dto := taskDTO(t, s.resolvedTaskStatus(t))
		applyTaskArchiveBlocker(&dto, archiveBlockers)
		metric := metrics[t.ExpID]
		dto.Tokens = tokenTotalDTO(metric.Tokens)
		// prefer the live in-memory heartbeat (fresher) and fall back to the
		// persisted max activity time (survives restarts) for run-duration display.
		dto.LastActivity = metric.LastActivity
		if live := s.engine.LastActivity(t.ID); live > dto.LastActivity {
			dto.LastActivity = live
		}
		dto.GoalsTotal = metric.Goals.Total
		dto.GoalsMet = metric.Goals.Met
		dto.InFlight = metric.RunningIntents
		dto.Findings = FindingSeverityDTO{
			Critical: metric.Findings.Critical,
			High:     metric.Findings.High,
			Medium:   metric.Findings.Medium,
			Low:      metric.Findings.Low,
		}
		dtos = append(dtos, dto)
	}
	writeJSON(w, 200, map[string]any{"tasks": dtos, "active": active})
}

func (s *Server) resolvedTaskStatus(t *Task) string {
	if t == nil {
		return "created"
	}
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Queued:
		return "queued"
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case t.llmStateSnapshot().FailoverState == "chain_exhausted" && s.engine.Started(t.ID):
		// LLM readiness is an execution dependency, not lifecycle state. Keeping
		// an exhausted task running lets the user edit/reset its chain directly.
		return "running"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	default:
		return "created"
	}
}

func (s *Server) setActive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.m.SetActive(req.ID) {
		writeErr(w, 404, "task not found")
		return
	}
	// resume the engine for the opened task (idempotent — no-op if already running).
	// 대기 중인 작업: 활성으로만 표시해 볼 수 있게 하고 엔진은 시작하지 않는다(동시 실행 제한을 지키고, reconcile이 빈자리를 채운다).
	if t, ok := s.m.Task(req.ID); ok && !t.lifecycleSnapshot().Queued {
		s.engine.Run(s.ctx, t)
	}
	writeJSON(w, 200, map[string]any{"active": req.ID})
}

// control pauses/resumes a task's autonomous execution (planner + workers).
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Action != "pause" && req.Action != "resume" {
		writeErr(w, 400, "action must be pause|resume")
		return
	}
	result, err := s.applyTaskControl(t, req.Action)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

// controlIntent pauses, resumes, or cancels one local worker intent. A running
// worker is always stopped before state mutation/cleanup, preventing tool output
// that arrives after the user action from recreating deleted blackboard records.
func (s *Server) controlIntent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 의도를 제어할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if err != nil || iid <= 0 {
		writeErr(w, 400, "bad intent id")
		return
	}
	var req struct {
		Action string `json:"action"`
		Reason string `json:"reason"` // cancel(삭제)일 때 필수: 삭제 이유
		Mode   string `json:"mode"`   // cancel 전용: soft(기본값, 소프트 삭제) | hard(영구 삭제, 이 작업만 가진 하위 작업까지 연쇄 삭제)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if req.Action != "pause" && req.Action != "resume" && req.Action != "cancel" {
		writeErr(w, 400, "action must be pause|resume|cancel")
		return
	}

	result, err := s.applyIntentControl(r.Context(), t, iid, req.Action, req.Reason, req.Mode)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

// rerunIntent는 성공하지 못한 의도 하나(blocked/exhausted/stopped)를 다시 실행한다. open으로
// 되돌리면 worker가 다시 할당받아 처음부터 실행한다(그래프에 이미 쓴 fact·finding·asset은
// 유지한다). 작업이 종료 상태이거나 일시 중지됐으면 함께 재개한다. '오류 난 work를 눌러 계속
// 실행'하는 기능이다. 네트워크·LLM이 잠깐 불안정해 blocked된 뒤 한 번에 재시도할 수 있다.
func restoreRerunIntent(t *Task, before *db.Node) error {
	if t == nil || before == nil {
		return fmt.Errorf("missing intent rollback snapshot")
	}
	if before.State == "blocked" && before.BlockedReason != "" {
		return t.Store.SetIntentBlockedReason(before.ID, before.BlockedReason)
	}
	return t.Store.SetIntentState(before.ID, before.State)
}

func (s *Server) rerunIntent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad intent id")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 의도를 재실행할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)
	before, err := t.Store.GetNode(iid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	reopened, err := t.Store.ReopenIntent(iid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !reopened {
		writeErr(w, 409, "이 의도는 재실행할 수 있는 상태가 아닙니다(blocked/exhausted/stopped만 재실행할 수 있습니다)")
		return
	}
	queued, err := s.admitTask(t, "resume")
	if err != nil {
		if rollbackErr := restoreRerunIntent(t, before); rollbackErr != nil {
			err = fmt.Errorf("%w; restore intent %d after admission failure: %v", err, iid, rollbackErr)
		}
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("[task] #%s 의도 #%d 다시 열림(재실행)", t.ID, iid)
	writeJSON(w, 200, map[string]any{"id": t.ID, "reopened": iid, "queued": queued})
}

// rerunBlocked 는 이 작업의 blocked 의도를 모두 일괄 재실행한다(네트워크·LLM 연결이 한 번
// 끊겨 여러 건이 blocked됐을 때 한 번에 재시도하기 좋다). open으로 되돌리고 작업을 재개하며,
// 다시 연 건수를 돌려준다.
func (s *Server) rerunBlocked(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 의도를 재실행할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)
	intents, err := t.Store.ListByKind(db.KindIntent, 1000000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	before := make([]*db.Node, 0)
	for _, intent := range intents {
		if intent.State == "blocked" {
			copy := *intent
			before = append(before, &copy)
		}
	}
	n, err := t.Store.ReopenBlockedIntents()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	queued := false
	if n > 0 {
		queued, err = s.admitTask(t, "resume")
		if err != nil {
			rollbackErrors := make([]string, 0)
			for _, intent := range before {
				if rollbackErr := restoreRerunIntent(t, intent); rollbackErr != nil {
					rollbackErrors = append(rollbackErrors, fmt.Sprintf("intent %d: %v", intent.ID, rollbackErr))
				}
			}
			if len(rollbackErrors) > 0 {
				err = fmt.Errorf("%w; restore blocked intents after admission failure: %s", err, strings.Join(rollbackErrors, "; "))
			}
			writeErr(w, 500, err.Error())
			return
		}
		log.Printf("[task] #%s blocked 의도 %d건 일괄 다시 열림", t.ID, n)
	}
	writeJSON(w, 200, map[string]any{"id": t.ID, "reopened": n, "queued": queued})
}

// getLLM returns the current LLM config (key never exposed).
func (s *Server) getLLM(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"configured":       s.llmOn,
		"provider":         s.llmCfg.Provider(),
		"model":            s.llmCfg.Model,
		"base_url":         s.llmCfg.BaseURL,
		"proxy":            s.llmCfg.Proxy,
		"key_set":          s.llmCfg.APIKey != "",
		"rate_per_second":  s.llmCfg.RatePerSecond,
		"rate_per_minute":  s.llmCfg.RatePerMinute,
		"context_window_k": s.llmCfg.ContextWindowK,
		"thinking_type":    s.llmCfg.ThinkingType,
		"reasoning_effort": s.llmCfg.ReasoningEffort,
	})
}

// setLLM configures the LLM at runtime. A blank api_key keeps the existing key.
func (s *Server) setLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider        string  `json:"provider"`
		Model           string  `json:"model"`
		BaseURL         string  `json:"base_url"`
		Proxy           string  `json:"proxy"`
		APIKey          string  `json:"api_key"`
		RatePerSecond   float64 `json:"rate_per_second"`
		RatePerMinute   float64 `json:"rate_per_minute"`
		ContextWindowK  int     `json:"context_window_k"`
		ThinkingType    string  `json:"thinking_type"`
		ReasoningEffort string  `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg := agent.ConfigFrom(req.Provider, req.Model, req.BaseURL, req.APIKey, req.Proxy)
	cfg.RatePerSecond, cfg.RatePerMinute = req.RatePerSecond, req.RatePerMinute
	cfg.ThinkingType = req.ThinkingType
	cfg.ReasoningEffort = req.ReasoningEffort
	if k := req.ContextWindowK; k > 0 { // 0 = keep default (200K); cap at 1M
		if k > 1000 {
			k = 1000
		}
		cfg.ContextWindowK = k
	}
	if cfg.APIKey == "" {
		s.cfgMu.Lock()
		cfg.APIKey = s.llmCfg.APIKey // keep existing key if not re-entered
		s.cfgMu.Unlock()
	}
	if cfg.APIKey == "" {
		writeErr(w, 400, "api_key required")
		return
	}
	// Validate provider construction before persisting it as the active profile.
	if _, err := cfg.NewProvider(); err != nil {
		writeErr(w, 400, "provider init failed: "+err.Error())
		return
	}
	if err := s.saveLLMConfig(cfg); err != nil {
		if errors.Is(err, errLegacyOAuthProfile) {
			writeErr(w, 400, legacyOAuthProfileMessage)
			return
		}
		writeErr(w, 500, "persist provider failed: "+err.Error())
		return
	}
	s.invalidateProfileAgents()
	if _, ok := s.loadLLMConfig(); !ok {
		writeErr(w, 500, "saved provider is unavailable")
		return
	}
	if err := s.applyLLM(cfg); err != nil {
		writeErr(w, 400, "provider init failed: "+err.Error())
		return
	}
	log.Printf("[engine] LLM configured via UI: %s / %s", cfg.Provider(), cfg.Model)
	s.getLLM(w, r)
}

// testLLM makes a real minimal completion to verify the config works.
func (s *Server) testLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider         string `json:"provider"`
		Model            string `json:"model"`
		BaseURL          string `json:"base_url"`
		Proxy            string `json:"proxy"`
		APIKey           string `json:"api_key"`
		ThinkingType     string `json:"thinking_type"`
		ReasoningEffort  string `json:"reasoning_effort"`
		ProfileID        *int64 `json:"profile_id"`         // 저장된 프로필을 테스트할 때 넣는다. api_key가 비면 그 프로필의 키를 쓴다
		Streaming        *bool  `json:"streaming"`          // 생략하면 스트리밍. 프로필을 저장할 때와 같은 기본값
		SessionHeaderKey string `json:"session_header_key"` // 비어 있지 않으면 테스트 요청에도 이 사용자 지정 세션 헤더를 붙인다. 값은 일회성 무작위 session id
		// AuthType 이 비면 profile_id 프로필의 인증 방식을 따른다.
		AuthType db.AuthType `json:"auth_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var stored *db.LLMProfile
	if req.ProfileID != nil {
		stored, _ = s.m.pg.ProfileByID(*req.ProfileID) // 못 읽으면 아래에서 키 없음으로 끝난다
	}
	authType := req.AuthType
	if authType == "" && stored != nil {
		authType = stored.AuthType
	}
	cfg := agent.ConfigFrom(profileFormat(authType, req.Provider), req.Model, req.BaseURL, req.APIKey, req.Proxy)
	// mirror production: send the SAME thinking params so a provider that rejects the
	// reasoning_effort/thinking field fails the test too (no false "test ok, run 400").
	cfg.ThinkingType = req.ThinkingType
	cfg.ReasoningEffort = req.ReasoningEffort
	// 같은 이유로 스트리밍 여부도 그 프로필의 선택을 따른다. 한쪽 방식만 지원하는 엔드포인트는
	// 세션에서 '테스트는 통과했는데 실제로는 안 돈다'는 것을 알게 되기 전에 여기서 드러나야 한다.
	if req.Streaming != nil {
		cfg.Stream = *req.Streaming
	}
	// 사용자 지정 세션 헤더 이름도 그 프로필을 따른다. 비어 있지 않으면 테스트 요청에도 이 헤더를
	// 붙인다(값은 일회성 무작위 session id, TestConnection 참고). opencode zen처럼
	// x-opencode-session을 반드시 요구하는 엔드포인트는 이 헤더가 없으면 바로 400을 돌려주므로
	// 테스트 경로에도 붙여야 한다. 그러지 않으면 '대화는 되는데 테스트는 400'이 된다.
	cfg.SessionHeaderKey = req.SessionHeaderKey
	// API 키 우선순위: 입력 폼 > 지정한 프로필에 저장된 키 > 전역 설정의 키.
	// 저장된 프로필의 키는 브라우저로 돌려보내지 않으므로 저장된 프로필을 테스트할 때는 폼이
	// 비어 있어 DB에서 읽어야 한다. 세션 헤더 이름도 같다: 폼에 없으면 저장된 프로필 값을 쓴다.
	if stored != nil && cfg.SessionHeaderKey == "" {
		cfg.SessionHeaderKey = stored.SessionHeaderKey
	}
	if authType.IsOAuth() {
		if msg := s.prepareSubscriptionTest(r.Context(), &cfg, stored, authType); msg != "" {
			writeJSON(w, 200, map[string]any{"ok": false, "error": msg})
			return
		}
	} else if stored != nil && cfg.APIKey == "" {
		cfg.APIKey = stored.APIKey
	}
	if !cfg.AuthType.IsOAuth() {
		if cfg.APIKey == "" {
			s.cfgMu.Lock()
			cfg.APIKey = s.llmCfg.APIKey
			s.cfgMu.Unlock()
		}
		if cfg.APIKey == "" {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "API 키를 입력하세요"})
			return
		}
	}
	// 재시도 파라미터는 연결 테스트에 넣지 않는다. 테스트에는 30s 강제 시간 초과가 있어서
	// 설정한 재시도 횟수·긴 간격을 더하면 멀쩡한 엔드포인트도 '시간 초과로 실패'가 된다.
	// 테스트가 보는 것은 '이 엔드포인트에 연결되는가'이고, 재시도 주기는 실제로 돌 때의 일이다.
	lat, reply, err := agent.TestConnection(r.Context(), cfg)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 모델의 실제 답을 돌려줘 '테스트 통과'의 근거를 남긴다. HTTP 200만이 아니라 모델이 실제로 답했음을 볼 수 있다.
	writeJSON(w, 200, map[string]any{
		"ok": true, "latency_ms": lat.Milliseconds(), "model": cfg.Model, "reply": truncateReply(reply),
	})
}

// prepareOAuthTest 는 연결 테스트 설정을 저장된 chatgpt_oauth 프로필의 토큰으로 바꾼다.
// 테스트할 수 없으면 사용자에게 보일 고정 문구를 돌려준다. 문구에 오류 원문을 싣지 않는다.
func (s *Server) prepareOAuthTest(ctx context.Context, cfg *agent.Config, stored *db.LLMProfile) string {
	if stored == nil {
		return chatGPTSaveFirstMessage
	}
	applyChatGPTOAuthRules(cfg, s.codexURL())
	src, err := s.oauth.connectedSource(ctx, stored.ID)
	if errors.Is(err, errOAuthNotConnected) {
		return chatGPTLoginRequiredMessage
	}
	if err != nil {
		log.Printf("[llm-test] LLM profile %d OAuth credentials unavailable: %v", stored.ID, err)
		return chatGPTCredentialsUnavailableMessage
	}
	cfg.OAuthTokens = src
	return ""
}

// truncateReply clips a connection-test reply for display. A model told to answer
// "OK" can still ramble (or think out loud); the UI only needs enough to show it
// really said something. Rune-based so multibyte text never splits mid-character.
func truncateReply(s string) string {
	const max = 200
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

type createTaskReq struct {
	Name                 string   `json:"name,omitempty"` // 선택 작업 이름. 생략하거나 비우면 이름 없음
	CategoryID           *int64   `json:"category_id,omitempty"`
	Description          string   `json:"description"`
	Goal                 string   `json:"goal"`
	LLMProfileID         *int64   `json:"llm_profile_id,omitempty"`    // 이 작업을 실행할 LLM 프로필. 생략하거나 null이면 활성 프로필
	LLMProfileIDs        []int64  `json:"llm_profile_ids,omitempty"`   // 순서 있는 작업 단위 프로필 체인. 첫 항목이 처음 적용된다
	SourceTaskIDs        []string `json:"source_task_ids,omitempty"`   // 직접 읽기 전용으로 이어받는 출처 작업만
	CompanyIDs           []int64  `json:"company_ids,omitempty"`       // 관련 기업 범위. 현재 기업 자산을 스냅숏으로 연결한다. 자산을 복사하거나 의도를 강제로 만들지 않는다
	TimeoutSeconds       int      `json:"timeout_seconds"`             // 작업 단위 시간 초과(초). 0이거나 생략하면 제한 없음
	PlanHeartbeatSeconds int      `json:"plan_heartbeat_seconds"`      // planner 하트비트 트리거 간격(초). 0이거나 생략하면 기본값 600(10min). 최솟값도 600이라 더 작으면 600으로 올린다
	SeedFirstIntent      *bool    `json:"seed_first_intent,omitempty"` // 만들 때 시드 의도 하나(내용=설명+목표)를 바로 넣어 worker가 첫 planner 회차를 기다리지 않고 시작하게 한다. 생략하거나 null이면 기본값 꺼짐(표준대로 먼저 계획한 뒤 실행). true를 명시해야 켠다(CTF처럼 work 하나로 풀리는 경우 시작 전 planner 회차를 줄일 수 있다).
	CoverageEnabled      *bool    `json:"coverage_enabled,omitempty"`  // 자산 커버리지 기능. 생략하거나 null이면 기본값 켜짐(true). false면 커버리지 계산·표시·범위 자동 누적을 끄고 add_task_scope/list_untested_assets를 숨긴다. 기업 연결에는 영향이 없다.
	// InterceptRules 는 작업 단위 자산 차단·허용 규칙이다(만들 때 입력하고 task_intercept_rules에 저장하며 전역 표에는 넣지 않는다).
	InterceptRules []taskInterceptRuleReq `json:"intercept_rules,omitempty"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		req.Description = "이름 없는 작업"
	}
	if len(req.LLMProfileIDs) == 0 && req.LLMProfileID != nil {
		req.LLMProfileIDs = []int64{*req.LLMProfileID}
	}
	if err := s.validateTaskProfileIDs(req.LLMProfileIDs); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.TimeoutSeconds < 0 {
		req.TimeoutSeconds = 0
	}
	if len(req.SourceTaskIDs) > db.MaxTaskSourceCount {
		writeErr(w, 400, fmt.Sprintf("관련 작업은 최대 %d개까지 고를 수 있습니다", db.MaxTaskSourceCount))
		return
	}
	sourceIDs := make([]int64, 0, len(req.SourceTaskIDs))
	seenSources := map[int64]bool{}
	for _, raw := range req.SourceTaskIDs {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id <= 0 || seenSources[id] {
			writeErr(w, 400, "관련 작업 id가 잘못됐거나 중복됐습니다")
			return
		}
		if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
			writeErr(w, 400, fmt.Sprintf("관련 작업 #%d이(가) 없습니다", id))
			return
		}
		seenSources[id] = true
		sourceIDs = append(sourceIDs, id)
	}
	companyIDs, err := db.NormalizeTaskCompanyIDs(req.CompanyIDs)
	if err != nil {
		writeErr(w, 400, fmt.Sprintf("관련 기업이 잘못됐습니다. 유효한 기업을 최대 %d개까지 고를 수 있습니다.", db.MaxTaskCompanyCount))
		return
	}
	req.CompanyIDs = companyIDs
	interceptRules, err := buildTaskInterceptRules(req.InterceptRules)
	if err != nil {
		writeErr(w, 400, "작업 단위 차단 규칙이 잘못됐습니다: "+err.Error())
		return
	}
	t, err := s.m.CreateTaskWithOptions(req.Description, req.Goal, db.TaskCreateOptions{
		Name: strings.TrimSpace(req.Name), CategoryID: req.CategoryID,
		SourceTaskIDs: sourceIDs, CompanyIDs: req.CompanyIDs, LLMProfileIDs: req.LLMProfileIDs,
		TimeoutSeconds: req.TimeoutSeconds, PlanHeartbeatSeconds: req.PlanHeartbeatSeconds,
		CoverageEnabled: req.CoverageEnabled,
		InterceptRules:  interceptRules,
	})
	if err != nil {
		if errors.Is(err, db.ErrTaskCategoryInvalid) || errors.Is(err, db.ErrTaskCategoryNotFound) {
			writeErr(w, 400, "작업 분류가 없거나 잘못됐습니다")
			return
		}
		if errors.Is(err, db.ErrTaskCompanyIDsInvalid) || errors.Is(err, db.ErrTaskCompanyNotFound) {
			writeErr(w, 400, "관련 기업이 없거나 잘못됐습니다")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("[task] 새 작업 #%s «%s» 목표: %s", t.ID, req.Description, req.Goal)
	// 만든 뒤 절차(seed + 시드 의도 + 백그라운드 목표 분해 + engine.Run)는 spawn_task와 같은 코드를 쓴다.
	// launchTask는 안에서 비동기로 돌아 UI를 막지 않는다. 목표 분해는 백그라운드에서 보이게 진행된다.
	s.launchTask(t, req.Description+" "+req.Goal, req.SeedFirstIntent != nil && *req.SeedFirstIntent)
	writeJSON(w, 201, taskDTO(t, s.resolvedTaskStatus(t)))
}

func (s *Server) validateTaskProfileIDs(ids []int64) error {
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return fmt.Errorf("LLM 프로필 id가 잘못됐거나 중복됐습니다")
		}
		seen[id] = true
		if _, ok := s.loadProfileConfig(id); !ok {
			return fmt.Errorf("LLM 프로필 #%d이(가) 없거나 API 키가 설정되지 않았습니다", id)
		}
	}
	return nil
}

func (s *Server) updateTaskLLMProfiles(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	// 어떤 수명 주기 상태(종료 상태 포함)에서도 체인을 바꿀 수 있다. 작업이 끝난 뒤에도 메인
	// 에이전트 대화는 이 체인을 쓰므로, 모델을 쓸 수 없을 때 체인을 못 바꾸면 완료된 작업의
	// 대화까지 막힌다.
	before := t.llmStateSnapshot()
	var req struct {
		LLMProfileIDs      []int64 `json:"llm_profile_ids"`
		ActiveLLMProfileID *int64  `json:"active_llm_profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if err := s.validateTaskProfileIDs(req.LLMProfileIDs); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	active := int64(0)
	if req.ActiveLLMProfileID != nil {
		active = *req.ActiveLLMProfileID
	}
	reopened, err := s.m.ReplaceTaskLLMProfiles(t.ID, req.LLMProfileIDs, active)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Profile edits affect only subsequent LLM calls. Existing in-flight calls
	// retain their provider. Concurrency reconciliation treats ActiveLLMCalls as a
	// live slot and postpones an unavailable task's pause/queue transition until
	// the call returns; a nudge lets an idle running task resume promptly.
	t.Notify()
	go s.reconcileConcurrency()
	llmState := t.llmStateSnapshot()
	result := map[string]any{
		"id": t.ID, "llm_profile_ids": llmState.ProfileIDs,
		"active_llm_profile_id": llmState.ActiveID,
		"llm_failover_state":    llmState.FailoverState, "reopened_intents": reopened,
	}
	if !sameOptionalID(before.ActiveID, llmState.ActiveID) {
		event := s.emitManualTaskLLMSwitch(t, before.ActiveID, llmState.ActiveID)
		result["switch_event"] = activityDTO(event)
	}
	writeJSON(w, 200, result)
}

var (
	reURL    = regexp.MustCompile(`https?://[^\s'"]+`)
	reIPPort = regexp.MustCompile(`\b((?:\d{1,3}\.){3}\d{1,3})(?::(\d{1,5}))?`)
	reDomain = regexp.MustCompile(`\b((?:[a-zA-Z0-9-]+\.)+[a-zA-Z]{2,})(?::(\d{1,5}))?`)
)

// parseTarget extracts a target (scheme, host, port) from free text, supporting
// full URLs, IP[:port] and domain[:port]. ok=false when nothing parseable.
func parseTarget(text string) (scheme, host string, port int, ok bool) {
	text = strings.TrimSpace(text)
	if m := reURL.FindString(text); m != "" {
		if u, err := url.Parse(m); err == nil && u.Hostname() != "" {
			scheme = strings.ToLower(u.Scheme)
			host = strings.ToLower(u.Hostname())
			port = portOr(u.Port(), defaultPort(scheme))
			return scheme, host, port, true
		}
	}
	if m := reIPPort.FindStringSubmatch(text); m != nil {
		host = m[1]
		port = portOr(m[2], 80)
		return schemeForPort(port), host, port, true
	}
	if m := reDomain.FindStringSubmatch(text); m != nil {
		host = strings.ToLower(m[1])
		if m[2] == "" {
			return "https", host, 443, true
		}
		port = portOr(m[2], 443)
		return schemeForPort(port), host, port, true
	}
	return "", "", 0, false
}

func portOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return d
}
func defaultPort(scheme string) int {
	if scheme == "http" {
		return 80
	}
	return 443
}
func schemeForPort(p int) string {
	if p == 443 || p == 8443 {
		return "https"
	}
	return "http"
}

// llmHost returns the host of the configured LLM endpoint (to keep it out of scope).
func (s *Server) llmHost() string {
	s.cfgMu.Lock()
	base := s.llmCfg.BaseURL
	s.cfgMu.Unlock()
	if base == "" {
		return ""
	}
	if u, err := url.Parse(base); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return ""
}

func (s *Server) seed(t *Task, text string) {
	scheme, host, port, ok := parseTarget(text)
	if !ok {
		log.Printf("[seed] task %s: %q에서 대상 host/IP를 파싱하지 못해 사이트를 만들지 않음(scope를 직접 설정해야 함)", t.ID, text)
		return
	}
	// P0-1 guard: never treat the configured LLM gateway as a target.
	if gw := s.llmHost(); gw != "" && host == gw {
		log.Printf("[seed] task %s: 대상 %q은(는) LLM 게이트웨이라 침투 테스트 대상으로 거부함", t.ID, host)
		return
	}

	u := scheme + "://" + host
	if !(scheme == "https" && port == 443) && !(scheme == "http" && port == 80) {
		u += ":" + strconv.Itoa(port)
	}
	var rootID int64
	if as := s.m.Assets(); as != nil {
		taskID, _ := strconv.ParseInt(t.ID, 10, 64)
		if net.ParseIP(host) != nil {
			rootID, _ = as.UpsertIP(db.UpsertIPReq{IP: host, TaskID: taskID})
		} else if scheme == "https" || scheme == "http" {
			rootID, _ = as.UpsertHTTPService(db.UpsertHTTPServiceReq{URL: u, TaskID: taskID})
		} else {
			rootID, _ = as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: host, TaskID: taskID})
		}
		if rootID > 0 {
			_ = as.SetTaskAssetSource(taskID, rootID, "task", "작업 설명이나 목표에서 초기화", nil)
		}
	}
	// anchor the seeded assets to this task's begin root as lineage/provenance
	// (the asset graph is global and shared; anchoring no longer gates reads).
	if rootID > 0 {
		if begin, _ := t.Store.OriginFactID(); begin > 0 {
			_ = t.Store.Anchor(begin, rootID)
		}
	}
	log.Printf("[seed] task %s: 대상 사이트 %s", t.ID, u)
	// 여기서 Notify하지 않는다. 첫 회차를 트리거할지는 engine.Run의 HasActiveIntent가 한곳에서
	// 정한다(시드 의도가 있는 작업은 첫 회차를 건너뛴다). seed는 Run보다 먼저 돌므로 여기서
	// Notify하면 채널에 버퍼로 쌓였다가 plannerLoop가 시작할 때 소비돼 Run의 관문을 우회하고,
	// 시드 작업이 첫 회차를 잘못 트리거한다.
}

// seedFirstIntent 는 작업을 만들 때 open 의도 하나(summary = 설명+목표)를 frontier에 넣어,
// worker가 planner 회차를 기다리지 않고 바로 할당받아 실행하게 한다. planner의 최상위 의도와
// 같은 모양이다: 시작점 fact에서 RelDerivedFrom으로 이어 fact 노드까지 추적할 수 있다.
// 최선 노력 방식이라 실패하면 보통의 planner 주도 흐름으로 돌아간다.
func (s *Server) seedFirstIntent(t *Task) {
	summary := fmt.Sprintf("작업 목표 달성: %s(작업: %s)", t.Goal, t.Description)
	id, err := t.Store.AddIntent(map[string]any{"summary": summary}, 8, nil, "seed")
	if err != nil {
		log.Printf("[seed] task %s: 시드 의도 넣기 실패: %v", t.ID, err)
		return
	}
	if origin, _ := t.Store.OriginFactID(); origin > 0 {
		_ = t.Store.Link(origin, db.RelDerivedFrom, id)
	}
	log.Printf("[seed] task %s: 시드 의도 #%d 넣음", t.ID, id)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	dto := taskDTO(t, s.resolvedTaskStatus(t))
	archiveBlockers, _ := s.m.PG().TaskArchiveBlockers()
	applyTaskArchiveBlocker(&dto, archiveBlockers)
	writeJSON(w, 200, dto)
}

// taskCoverage returns a task's rough asset test coverage (denominator/tested/backlog).
func (s *Server) taskCoverage(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소를 사용할 수 없습니다")
		return
	}
	// 자산 커버리지 기능이 꺼져 있으면 바로 {enabled:false}를 돌려준다. 프런트는 이를 보고 커버리지 카드·진행률을 숨긴다.
	if !t.CoverageEnabled {
		writeJSON(w, 200, &db.Coverage{Enabled: false, ByType: []db.CoverageByType{}})
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	cov, err := as.TaskCoverageWithSources(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	cov.Enabled = true
	writeJSON(w, 200, cov)
}

// taskCoverageGraph 는 작업의 포스 기반(force-directed) 자산 커버리지 그래프를 돌려준다:
// 범위 안의 모든 자산(유형별)과 연결용 루트 도메인·기업 노드. 각각 tested/in_scope를 가진다.
func (s *Server) taskCoverageGraph(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소를 사용할 수 없습니다")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	g, err := as.BuildCoverageGraph(taskID, t.ExpID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, g)
}

// taskAssetRefs 는 이 작업에서 주어진 자산 id에 고정된 의도·사실·발견 사항을 돌려준다.
// 커버리지 그래프 노드 서랍의 '관련 의도 / 관련 사실'에 쓴다.
func (s *Server) taskAssetRefs(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	assetID, _ := strconv.ParseInt(r.URL.Query().Get("asset_id"), 10, 64)
	if assetID <= 0 {
		writeErr(w, 400, "asset_id를 입력하세요")
		return
	}
	refs, err := t.Store.AssetRefsWithSources(assetID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	intents := []CoverageAssetRefDTO{}
	facts := []CoverageAssetRefDTO{}
	findings := []CoverageAssetRefDTO{}
	for _, ref := range refs {
		dto := coverageAssetRefDTO(ref)
		switch ref.Kind {
		case "intent":
			intents = append(intents, dto)
		case "fact":
			facts = append(facts, dto)
		case "finding":
			findings = append(findings, dto)
		}
	}
	writeJSON(w, 200, map[string]any{"intents": intents, "facts": facts, "findings": findings})
}

// taskScopeList returns a task's scope rows (coverage denominator sources).
func (s *Server) taskScopeList(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소를 사용할 수 없습니다")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	rows, err := as.ListTaskScopeWithSources(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"scope": rows})
}

func (s *Server) taskScopeAdd(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소를 사용할 수 없습니다")
		return
	}
	var body struct {
		Kind   string `json:"kind"`
		Value  string `json:"value"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	ts, err := as.AddAgentScope(taskID, body.Kind, body.Value, body.Reason, "manual")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, ts)
}

func (s *Server) taskScopeDelete(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소를 사용할 수 없습니다")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	scopeID, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid scope id")
		return
	}
	deleted, err := as.DeleteTaskScope(taskID, scopeID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !deleted {
		writeErr(w, 404, "scope row not found")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) frontier(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, []any{})
		return
	}
	fr, _ := t.Store.Frontier(atoiDefault(r.URL.Query().Get("limit"), 100))
	writeJSON(w, 200, taskNodeDTOs(fr))
}

func (s *Server) findings(w http.ResponseWriter, r *http.Request) {
	// task 파라미터가 없으면 전역 '발견 사항' 화면이다: 독립 findings 표에서 읽는다(작업을 지워도 finding은 남는다).
	// task 파라미터가 있으면 그 작업만 본다(작업 개요·발견 사항 탭용): exploration_nodes에서 읽는다(작업이 있으면 노드도 있다).
	q := r.URL.Query()
	taskParam := q.Get("task")
	if taskParam == "" {
		// page/limit가 있으면 서버 쪽 페이지 나누기 {items,total,...}, 없으면 배열 그대로(대시보드
		// 집계용, intents 엔드포인트의 호환 방식과 같다). 필터·정렬은 모두 SQL에서 한다.
		if q.Get("page") == "" && q.Get("limit") == "" {
			fs, _ := s.m.pg.ListFindings(500)
			assets := s.resolveFindingAssets(fs)
			out := make([]FindingDTO, 0, len(fs))
			for _, f := range fs {
				out = append(out, findingFromDB(f, assets))
			}
			writeJSON(w, 200, out)
			return
		}
		page := findingPaginationParam(q.Get("page"), 1, 0)
		limit := findingPaginationParam(q.Get("limit"), 20, 200)
		fs, total, err := s.m.pg.ListFindingsPage(findingFilterFromQuery(q), page, limit)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		assets := s.resolveFindingAssets(fs)
		out := make([]FindingDTO, 0, len(fs))
		for _, f := range fs {
			out = append(out, findingFromDB(f, assets))
		}
		writeJSON(w, 200, map[string]any{"items": out, "total": total, "page": page, "page_size": limit})
		return
	}
	t := s.m.ResolveTask(taskParam)
	if t == nil {
		writeJSON(w, 200, []any{})
		return
	}
	f, err := t.Store.ListByKind(db.KindFinding, 200)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tid, _ := strconv.ParseInt(t.ID, 10, 64)
	meta, err := s.m.pg.FindingMetaByNodeID(tid)
	if err != nil {
		log.Printf("[findings] task=%s meta: %v", t.ID, err)
	}
	var aidSet []int64
	for _, m := range meta {
		aidSet = append(aidSet, m.AssetIDs...)
	}
	assets := s.resolveAssetIDs(aidSet)
	out := findingDTOsForTask(t, f, meta, assets)

	// Related-task findings are a live, read-only view. Provenance metadata lets
	// the task UI suppress mutation affordances while retaining stable ids.
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, source := range sources {
		nodes, listErr := source.Store.ListByKind(db.KindFinding, 200)
		if listErr != nil {
			writeErr(w, 500, listErr.Error())
			return
		}
		for _, node := range nodes {
			node.SourceTaskID = source.Task.TaskID
			node.Inherited = true
		}
		sourceMeta, metaErr := s.m.pg.FindingMetaByNodeID(source.Task.TaskID)
		if metaErr != nil {
			log.Printf("[findings] source_task=%d meta: %v", source.Task.TaskID, metaErr)
		}
		var sourceAssetIDs []int64
		for _, item := range sourceMeta {
			sourceAssetIDs = append(sourceAssetIDs, item.AssetIDs...)
		}
		out = append(out, findingDTOsForOwner(
			i64s(source.Task.TaskID),
			source.Task.Description,
			nodes,
			sourceMeta,
			s.resolveAssetIDs(sourceAssetIDs),
		)...)
	}
	writeJSON(w, 200, out)
}

// normFilter maps the frontend's "all" sentinel (and empty) to "" so the DB layer
// treats it as no filter.
func normFilter(v string) string {
	if v == "all" {
		return ""
	}
	return v
}

// resolveFindingAssets loads every asset anchored by the given findings, keyed by
// id, so each finding DTO can render its assets' labels.
func (s *Server) resolveFindingAssets(fs []*db.DBFinding) map[int64]*db.Asset {
	var ids []int64
	for _, f := range fs {
		ids = append(ids, f.AssetIDs...)
	}
	return s.resolveAssetIDs(ids)
}

// resolveAssetIDs de-dupes ids and loads their asset rows into an id→asset map.
func (s *Server) resolveAssetIDs(ids []int64) map[int64]*db.Asset {
	assets := map[int64]*db.Asset{}
	seen := map[int64]bool{}
	var uniq []int64
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return assets
	}
	list, err := s.m.pg.Assets().GetByIDs(uniq)
	if err != nil {
		log.Printf("[findings] resolve assets: %v", err)
	}
	for _, a := range list {
		assets[a.ID] = a
	}
	return assets
}

// findingStats 는 페이지로 나뉜 '발견 사항' 화면에 표 전체 집계(통계 카드 + 취약점 분류 필터)를 준다.
func (s *Server) findingStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.m.pg.FindingStats()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

// getFinding returns one finding by its standalone-table id (DTO finding_id),
// with anchored assets resolved for display.
func (s *Server) getFinding(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	assets := s.resolveAssetIDs(f.AssetIDs)
	dto := findingFromDB(f, assets)
	if contextTaskID := strings.TrimSpace(r.URL.Query().Get("context_task")); contextTaskID != "" {
		contextTask := s.m.ResolveTask(contextTaskID)
		if contextTask == nil {
			writeErr(w, 404, "context task not found")
			return
		}
		sourceTaskID, inherited, allowed := findingProvenanceInTask(contextTask, f.TaskID)
		if !allowed {
			writeErr(w, 404, "finding not available in task context")
			return
		}
		dto.SourceTaskID = sourceTaskID
		dto.Inherited = inherited
	}
	writeJSON(w, 200, dto)
}

// findingsExport 는 '발견 사항' 화면의 취약점을 내보낸다.
//
//	scope   = filtered(화면 필터를 그대로 씀) | all(전체) | selected(선택한 ids)
//	format  = md-single(.md 하나로 합침) | md-zip(취약점마다 .md 하나, zip으로 묶음)
//	          | csv | json
//	ids     = 쉼표로 구분한 finding id(scope=selected일 때 필수)
//	필터 파라미터 severity/status/vulnclass/task_id/q/sort는 목록 API와 같다(scope=filtered에서 씀).
func (s *Server) findingsExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	format := q.Get("format")

	var ids []int64
	var filter db.FindingFilter
	switch scope {
	case "selected":
		for _, part := range strings.Split(q.Get("ids"), ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				writeErr(w, 400, "bad finding id: "+part)
				return
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			writeErr(w, 400, "no findings selected")
			return
		}
	case "all":
		// 빈 filter는 조건을 하나도 붙이지 않는다.
	case "filtered", "":
		filter = findingFilterFromQuery(q)
	default:
		writeErr(w, 400, "bad scope: "+scope)
		return
	}

	fs, err := s.m.pg.ListFindingsForExport(filter, ids)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	stage, err := os.MkdirTemp("", "artex-finding-export-")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.RemoveAll(stage)
	if err = s.evidenceStore().StageFindingsExport(r.Context(), fs, stage, format == "md-zip"); err != nil {
		evidenceError(w, err)
		return
	}
	now := time.Now()
	stamp := now.Format("20060102-150405")
	setDownload := func(contentType, filename string) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	}

	switch format {
	case "md-single":
		setDownload("text/markdown; charset=utf-8", "findings-"+stamp+".md")
		_, _ = w.Write([]byte(report.FindingsMarkdown(fs, now)))
	case "md-zip":
		path := filepath.Join(stage, "findings.zip")
		if err := buildFindingsEvidenceZip(path, fs, stage, now); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		file, err := os.Open(path)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer file.Close()
		setDownload("application/zip", "findings-"+stamp+".zip")
		http.ServeContent(w, r, "findings.zip", now, file)
	case "csv":
		setDownload("text/csv; charset=utf-8", "findings-"+stamp+".csv")
		_, _ = w.Write(report.FindingsCSV(fs))
	case "json":
		assets := s.resolveFindingAssets(fs)
		out := make([]FindingDTO, 0, len(fs))
		for _, f := range fs {
			for i := range f.TrafficBindings {
				f.TrafficBindings[i].Snapshot.ReqHead = ""
				f.TrafficBindings[i].Snapshot.RespHead = ""
			}
			out = append(out, findingFromDB(f, assets))
		}
		setDownload("application/json; charset=utf-8", "findings-"+stamp+".json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	default:
		writeErr(w, 400, "bad format: "+format)
	}
}

func findingProvenanceInTask(contextTask *Task, findingTaskID *int64) (sourceTaskID string, inherited, allowed bool) {
	if contextTask == nil || findingTaskID == nil {
		return "", false, false
	}
	contextID, err := strconv.ParseInt(contextTask.ID, 10, 64)
	if err != nil {
		return "", false, false
	}
	if *findingTaskID == contextID {
		return "", false, true
	}
	for _, sourceID := range contextTask.lifecycleSnapshot().SourceTaskIDs {
		if sourceID == *findingTaskID {
			return i64s(sourceID), true, true
		}
	}
	return "", false, false
}

// findingLineage returns the exploration sub-DAG from the task root to this
// finding's node — the finding node + all its ancestors + edges among them — so
// the detail page can show "how this finding was reached". Empty {nodes,edges}
// when the finding has no node/task (e.g. the originating task was deleted).
func (s *Server) findingLineage(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	empty := map[string]any{"nodes": []any{}, "edges": []any{}}
	if f.NodeID == nil || f.TaskID == nil {
		writeJSON(w, 200, empty)
		return
	}
	t := s.m.ResolveTask(i64s(*f.TaskID))
	if t == nil {
		writeJSON(w, 200, empty)
		return
	}
	nodes, edges, err := t.Store.FindingLineage(*f.NodeID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"nodes": taskNodeDTOs(nodes), "edges": edgeDTOs(edges)})
}

// patchFinding partially updates a finding: any subset of {status, severity}.
// The id is the standalone findings-table id (DTO finding_id). Severity edits are
// mirrored onto the originating exploration node so the per-task view stays in
// sync. Returns the updated finding DTO.
func (s *Server) patchFinding(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	var body struct {
		Status    *string `json:"status"`
		Severity  *string `json:"severity"`
		Name      *string `json:"name"`
		VulnClass *string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if body.Status == nil && body.Severity == nil && body.Name == nil && body.VulnClass == nil {
		writeErr(w, 400, "nothing to update: provide status/severity/name/vulnclass")
		return
	}
	if body.Status != nil {
		if !db.ValidFindingStatus(*body.Status) {
			writeErr(w, 400, "bad status: "+*body.Status)
			return
		}
		// 알림이 붙은 버전을 쓴다: 상태 변경과 '상태 변경 알림 이벤트'를 한 트랜잭션에서 저장해
		// 상태는 바뀌었는데 알림 이벤트가 사라지는 틈을 없앤다. 이벤트 등록 실패는 상태 변경에
		// 영향을 주지 않으므로 로그만 남기고 호출자에게 오류를 돌려주지 않는다.
		from, found, notified, err := s.m.pg.SetFindingStatusWithNotify(r.Context(), id, *body.Status)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if !found {
			writeErr(w, 404, "finding not found")
			return
		}
		if !notified && from != *body.Status {
			log.Printf("[notify] 상태 변경 이벤트 등록 안 됨 finding=%d %s→%s(상태는 바뀜)", id, from, *body.Status)
		}
	}
	if body.Severity != nil {
		if !db.ValidSeverity(*body.Severity) {
			writeErr(w, 400, "bad severity: "+*body.Severity)
			return
		}
		n, err := s.m.pg.SetFindingSeverity(id, *body.Severity)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if n == 0 {
			writeErr(w, 404, "finding not found")
			return
		}
	}
	if body.Name != nil {
		n, err := s.m.pg.SetFindingName(id, strings.TrimSpace(*body.Name))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if n == 0 {
			writeErr(w, 404, "finding not found")
			return
		}
	}
	if body.VulnClass != nil {
		n, err := s.m.pg.SetFindingVulnClass(id, strings.TrimSpace(*body.VulnClass))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if n == 0 {
			writeErr(w, 404, "finding not found")
			return
		}
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	writeJSON(w, 200, findingFromDB(f, s.resolveAssetIDs(f.AssetIDs)))
}

// deleteFinding removes a finding (findings row + originating exploration node).
// id is the standalone findings-table id (DTO finding_id).
func (s *Server) deleteFinding(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	n, err := s.m.pg.DeleteFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if n == 0 {
		writeErr(w, 404, "finding not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
}

func (s *Server) intents(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		// Back-compat: bare list shape when the task can't be resolved.
		writeJSON(w, 200, []any{})
		return
	}
	q := r.URL.Query()
	limit := min(atoiDefault(q.Get("limit"), 300), 500)
	// No paging params → preserve the legacy bare-array response so existing callers
	// (and the poll) keep working unchanged.
	if q.Get("before") == "" && q.Get("page") == "" {
		in, err := t.Store.ListByKind(db.KindIntent, limit)
		if err != nil {
			log.Printf("[intents] task=%s limit=%d: %v", t.ID, limit, err)
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, taskNodeDTOs(in))
		return
	}
	// Paged form: ?before=<id> (or ?page as a marker) → {items, has_more} so the
	// worker session list can reach past the old fixed 300 boundary on scroll. It
	// also includes immutable intent results from directly related tasks; those
	// rows never participate in this task's frontier or claim path.
	before := int64(atoiDefault(q.Get("before"), 0))
	in, hasMore, err := taskIntentHistoryPage(t.Store, before, limit, false, 0)
	if err != nil {
		log.Printf("[intents] task=%s before=%d limit=%d: %v", t.ID, before, limit, err)
		writeErr(w, 500, err.Error())
		return
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, source := range sources {
		items, sourceMore, sourceErr := taskIntentHistoryPage(source.Store, before, limit, true, source.Task.TaskID)
		if sourceErr != nil {
			writeErr(w, 500, sourceErr.Error())
			return
		}
		in = append(in, items...)
		hasMore = hasMore || sourceMore
	}
	sort.Slice(in, func(i, j int) bool { return in[i].ID > in[j].ID })
	if len(in) > limit {
		in = in[:limit]
		hasMore = true
	}
	writeJSON(w, 200, map[string]any{"items": taskNodeDTOs(in), "has_more": hasMore})
}

func inheritedIntentResult(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// inheritedGraphSnapshot exposes immutable source-task history without leaking
// work that is still open or running. Edges are filtered with the nodes so the
// response cannot contain dangling ids that reveal hidden live intent topology.
func inheritedGraphSnapshot(nodes []*db.Node, edges []db.Edge, sourceTaskID int64) ([]*db.Node, []db.Edge) {
	visible := make(map[int64]struct{}, len(nodes))
	filteredNodes := make([]*db.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil || (node.Kind == db.KindIntent && !inheritedIntentResult(node.State)) {
			continue
		}
		node.SourceTaskID = sourceTaskID
		node.Inherited = true
		visible[node.ID] = struct{}{}
		filteredNodes = append(filteredNodes, node)
	}
	filteredEdges := make([]db.Edge, 0, len(edges))
	for _, edge := range edges {
		if _, ok := visible[edge.From]; !ok {
			continue
		}
		if _, ok := visible[edge.To]; !ok {
			continue
		}
		filteredEdges = append(filteredEdges, edge)
	}
	return filteredNodes, filteredEdges
}

// taskIntentHistoryPage returns one newest-first page for an exploration.
// A source may have many open/running intents, so keep paging until enough
// historical results are collected instead of leaking its frontier into the UI.
func taskIntentHistoryPage(store *db.ExplorationStore, before int64, limit int, inherited bool, sourceTaskID int64) ([]*db.Node, bool, error) {
	if !inherited {
		return store.ListByKindPage(db.KindIntent, before, limit)
	}
	batch := max(limit, 300)
	cursor := before
	out := make([]*db.Node, 0, limit+1)
	for {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil {
			return nil, false, err
		}
		for _, node := range page {
			if !inheritedIntentResult(node.State) {
				continue
			}
			node.SourceTaskID = sourceTaskID
			node.Inherited = true
			out = append(out, node)
			if len(out) > limit {
				return out[:limit], true, nil
			}
		}
		if !more || len(page) == 0 {
			return out, false, nil
		}
		cursor = page[len(page)-1].ID
	}
}

// explorationGraph returns the whole exploration chain (task graph) as nodes+edges.
func (s *Server) explorationGraph(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{"nodes": []any{}, "edges": []any{}})
		return
	}
	nodes, err := t.Store.Nodes(2000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	edges, err := t.Store.Edges(5000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, source := range sources {
		sourceNodes, nodeErr := source.Store.Nodes(2000)
		if nodeErr != nil {
			writeErr(w, 500, nodeErr.Error())
			return
		}
		sourceEdges, edgeErr := source.Store.Edges(5000)
		if edgeErr != nil {
			writeErr(w, 500, edgeErr.Error())
			return
		}
		sourceNodes, sourceEdges = inheritedGraphSnapshot(sourceNodes, sourceEdges, source.Task.TaskID)
		nodes = append(nodes, sourceNodes...)
		edges = append(edges, sourceEdges...)
	}
	writeJSON(w, 200, map[string]any{"nodes": taskNodeDTOs(nodes), "edges": edgeDTOs(edges)})
}

// explorationNodes 는 활동 피드에 이 작업 자신의 탐색 노드를 페이지로 나뉜 시계열로 준다
// (?order=asc가 아니면 최신순). kind·state와 payload 부분 문자열로 거를 수 있다. 이어받은
// 노드는 일부러 뺀다: 피드는 이 작업이 지금 하는 일을 보여 주고, 출처 작업의 저장소를 넘나들며
// 페이지를 나누면 커서가 의미를 잃는다.
// 응답에는 페이지에 닿는 엣지와 그 엣지가 가리키는 이웃 노드도 담아, 각 행이 어디서 왔고
// 무엇을 만들었는지 보여 줄 수 있게 한다.
func (s *Server) explorationNodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 1)
	size := atoiDefault(q.Get("size"), 20)
	t := s.m.ResolveTask(q.Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{
			"items": []any{}, "total": 0, "page": page, "size": size,
			"edges": []any{}, "refs": map[string]any{},
		})
		return
	}
	filter := db.NodeFilter{
		Kinds:  csvValues(q.Get("kind")),
		States: csvValues(q.Get("state")),
		Query:  q.Get("q"),
		Asc:    q.Get("order") == "asc",
	}
	nodes, total, err := t.Store.NodesPage(filter, page, size)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ids := make([]int64, 0, len(nodes))
	onPage := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
		onPage[n.ID] = true
	}
	edges, err := t.Store.EdgesTouching(ids)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	neighbourSet := map[int64]bool{}
	for _, e := range edges {
		if !onPage[e.From] {
			neighbourSet[e.From] = true
		}
		if !onPage[e.To] {
			neighbourSet[e.To] = true
		}
	}
	neighbourIDs := make([]int64, 0, len(neighbourSet))
	for id := range neighbourSet {
		neighbourIDs = append(neighbourIDs, id)
	}
	neighbours, err := t.Store.NodesByIDs(neighbourIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	refs := make(map[string]TaskNodeDTO, len(neighbours))
	for _, n := range neighbours {
		refs[i64s(n.ID)] = taskNodeDTO(n)
	}
	writeJSON(w, 200, map[string]any{
		"items":  taskNodeDTOs(nodes),
		"total":  total,
		"page":   page,
		"size":   size,
		"edges":  edgeDTOs(edges),
		"refs":   refs,
		"assets": s.nodeAnchoredAssets(t, append(ids, neighbourIDs...)),
	})
}

// nodeAnchoredAssets 는 주어진 노드들의 exploration_anchors를 화면에 바로 쓸 자산 라벨로
// 바꿔 노드 id별로 돌려준다. 앵커는 활동 피드의 출처 표시용 장식이라, 여기서 실패해도 호출자의
// 페이지를 잃게 하면 안 된다. 그래서 오류는 로그로 남기고 '자산 없음'으로 처리한다.
func (s *Server) nodeAnchoredAssets(t *Task, nodeIDs []int64) map[string][]FindingAssetDTO {
	out := map[string][]FindingAssetDTO{}
	anchors, err := t.Store.NodeAssets(nodeIDs)
	if err != nil {
		log.Printf("[broadcast] node assets: %v", err)
		return out
	}
	var flat []int64
	for _, ids := range anchors {
		flat = append(flat, ids...)
	}
	assets := s.resolveAssetIDs(flat)
	for nodeID, ids := range anchors {
		if dtos := findingAssetDTOs(ids, assets); len(dtos) > 0 {
			out[i64s(nodeID)] = dtos
		}
	}
	return out
}

// csvValues splits a comma-separated query parameter, dropping empty entries.
func csvValues(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// activity returns the worker execution step log (incremental via ?since=seq).
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "cursor": 0})
		return
	}
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 300)
	var intentPtr *int64
	if iv := r.URL.Query().Get("intent"); iv != "" {
		if n, err := strconv.ParseInt(iv, 10, 64); err == nil {
			intentPtr = &n
		}
	}
	var (
		items  []db.Activity
		cursor int64
		err    error
	)
	if intentPtr != nil {
		items, cursor, err = t.Store.ActivityListWithSources(*intentPtr, since, limit)
	} else {
		items, cursor, err = t.Store.ActivityList(nil, since, limit)
	}
	if err != nil {
		log.Printf("[activity] task=%s since=%d limit=%d intent=%v: %v", t.ID, since, limit, intentPtr, err)
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": activityDTOs(items), "cursor": cursor})
}

// parseActivitySession maps a stable session key (main | plan | intent:<ID>) to a
// DB session filter. Goal Agent + Planner both live under worker="planner" (the
// single Plan session); a Worker session is one intent, keyed by its node id.
func parseActivitySession(sess string) (db.ActivitySessionFilter, bool) {
	switch {
	case sess == "" || sess == "main":
		// bare "main" = the current segment; caller resolves MainSeg via the store.
		return db.ActivitySessionFilter{Main: true}, true
	case strings.HasPrefix(sess, "main:"):
		seg, err := strconv.Atoi(strings.TrimPrefix(sess, "main:"))
		if err != nil || seg < 0 {
			return db.ActivitySessionFilter{}, false
		}
		return db.ActivitySessionFilter{Main: true, MainSeg: &seg}, true
	case sess == "plan":
		return db.ActivitySessionFilter{Worker: "planner"}, true
	case strings.HasPrefix(sess, "intent:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(sess, "intent:"), 10, 64)
		if err != nil {
			return db.ActivitySessionFilter{}, false
		}
		return db.ActivitySessionFilter{NodeID: &id}, true
	}
	return db.ActivitySessionFilter{}, false
}

// activitySessionStore resolves a worker session against the current task and
// its direct sources. Planner/main always stay local; inherited intent sessions
// are immutable history and use the source exploration only for reads.
func activitySessionStore(t *Task, filter db.ActivitySessionFilter) (*db.ExplorationStore, int64, error) {
	if filter.NodeID == nil {
		return t.Store, 0, nil
	}
	node, err := t.Store.GetNodeWithSources(*filter.NodeID)
	if err != nil {
		return nil, 0, err
	}
	if node == nil || node.Kind != db.KindIntent {
		return nil, 0, nil
	}
	if !node.Inherited {
		return t.Store, 0, nil
	}
	if !inheritedIntentResult(node.State) {
		return nil, 0, nil
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		return nil, 0, err
	}
	for _, source := range sources {
		if source.Task.TaskID == node.SourceTaskID {
			return source.Store, source.Task.TaskID, nil
		}
	}
	return nil, 0, nil
}

// activityHistory serves one reverse-paginated page of a session's activity history.
// The latest page (no ?before) opens a session; ?before=<id> pulls the older page on
// scroll-up. snapshot_cursor is the TASK-level max id at query time — the client uses
// it to open the single task SSE at since=snapshot_cursor so history (id<=cursor) and
// the live tail (id>cursor) meet with no gap and no overlap.
func (s *Server) activityHistory(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	q := r.URL.Query()
	sess := q.Get("session")
	filter, ok := parseActivitySession(sess)
	if !ok {
		writeErr(w, 400, "bad session")
		return
	}
	if filter.Main && filter.MainSeg == nil { // bare "main" → the current segment
		seg, err := t.Store.CurrentMainSeg()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		filter.MainSeg = &seg
	}
	before := int64(atoiDefault(q.Get("before"), 0))
	limit := min(atoiDefault(q.Get("limit"), 200), 500) // cap so one request can't pull an unbounded slice
	store, sourceTaskID, err := activitySessionStore(t, filter)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if store == nil {
		writeErr(w, 404, "session not found")
		return
	}
	// The browser tails only this task's broadcaster. Keep the SSE join cursor
	// local even when the opened worker transcript comes from a related task.
	snapshot, err := t.Store.ActivityMaxID()
	if err != nil {
		log.Printf("[activity/history] task=%s session=%s snapshot: %v", t.ID, sess, err)
		writeErr(w, 500, err.Error())
		return
	}
	var items []db.Activity
	var hasMore bool
	if sourceTaskID > 0 {
		// Source sessions are readable only while the intent remains terminal. The
		// DB query also removes model reasoning/accounting rows from inherited data.
		items, hasMore, err = store.ActivityPageForTerminalIntent(*filter.NodeID, before, limit)
	} else {
		items, hasMore, err = store.ActivityPage(filter, before, limit)
	}
	if err != nil {
		log.Printf("[activity/history] task=%s session=%s before=%d limit=%d: %v", t.ID, sess, before, limit, err)
		writeErr(w, 500, err.Error())
		return
	}
	if sourceTaskID > 0 {
		for i := range items {
			items[i].SourceTaskID = sourceTaskID
			items[i].Inherited = true
		}
	}
	earliest := before
	if len(items) > 0 {
		earliest = items[0].ID
	}
	writeJSON(w, 200, map[string]any{
		"items":           activityDTOs(items),
		"snapshot_cursor": snapshot,
		"earliest_cursor": earliest,
		"has_more":        hasMore,
	})
}

// tokenStats returns per-worker token usage (input/output/cache read/write) for a
// task — main agent, planner, and each work#N.
func (s *Server) tokenStats(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{
			"workers":  []db.TokenUsage{},
			"sessions": []db.SessionTokenUsage{},
			"total":    tokenTotalDTO(db.TokenUsage{}),
		})
		return
	}
	stats, err := t.Store.TokenStatsByWorker()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sessions, err := t.Store.TokenStatsBySession()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	total, err := t.Store.TokenTotal() // whole-task total (all agents)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"workers": stats, "sessions": sessions, "total": tokenTotalDTO(total)})
}

// tokenDailyStats returns global token consumption aggregated by calendar day
// (UTC) across all tasks for the past ?days=N days (default 30).
func (s *Server) tokenDailyStats(w http.ResponseWriter, r *http.Request) {
	days := atoiDefault(r.URL.Query().Get("days"), 30)
	if s.m.pg == nil {
		writeJSON(w, 200, []any{})
		return
	}
	buckets, err := s.m.pg.TokenDailyAll(days)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if buckets == nil {
		buckets = []db.DailyTokenBucket{}
	}
	writeJSON(w, 200, buckets)
}

// conversationTokens returns per-conversation token summaries so the dashboard can
// merge chat (conversation) usage into its per-profile / daily token stats — which
// otherwise count only task (exploration) usage.
func (s *Server) conversationTokens(w http.ResponseWriter, r *http.Request) {
	if s.m.pg == nil {
		writeJSON(w, 200, []any{})
		return
	}
	rows, err := s.m.pg.ConversationTokenSummaries()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rows)
}

// streamActivity is the live SSE tail: it replays history after ?since=<seq>, then
// pushes each newly appended activity for the task. The seq cursor makes history +
// live join gap-free; on reconnect the client passes its last seq to catch any
// dropped events. Optional ?intent=<id> scopes the stream to one worker session.
// getLogs returns recent backend log lines (Seq > since), newest-last.
func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 500)
	lines, cursor := logSink.recent(since, limit)
	writeJSON(w, 200, map[string]any{"items": lines, "cursor": cursor})
}

// getLogsHistory returns older log lines from the DB (before a given db_id).
// GET /api/logs/history?before=<db_id>&limit=200
// Returns {items:[LogLine], has_more: bool}.
func (s *Server) getLogsHistory(w http.ResponseWriter, r *http.Request) {
	if s.m.pg == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "has_more": false})
		return
	}
	before := int64(atoiDefault(r.URL.Query().Get("before"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	if limit > 500 {
		limit = 500
	}
	// If no before given, return the most recent DB rows (mirrors ring restore).
	var (
		rows []*db.DBLog
		err  error
	)
	if before <= 0 {
		rows, err = s.m.pg.RecentLogs(limit)
	} else {
		rows, err = s.m.pg.ListLogsBefore(before, limit)
	}
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	items := make([]LogLine, 0, len(rows))
	for _, r := range rows {
		items = append(items, LogLine{
			DBID:  r.ID,
			TS:    r.CreatedAt.Format(time.RFC3339),
			Level: r.Level,
			Tag:   r.Tag,
			Text:  r.Text,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items, "has_more": len(rows) == limit})
}

// streamLogs is the live SSE tail of the backend log: replays history after
// ?since=<seq>, then pushes each new line.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := logSink.subscribe()
	defer unsub()
	send := func(l LogLine) {
		b, _ := json.Marshal(l)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	lines, cursor := logSink.recent(since, 1000)
	for _, l := range lines {
		send(l)
	}
	if cursor > since {
		since = cursor
	}
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case l, ok := <-ch:
			if !ok {
				return
			}
			if l.Seq <= since {
				continue
			}
			since = l.Seq
			send(l)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) streamActivity(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	var intentPtr *int64
	if iv := r.URL.Query().Get("intent"); iv != "" {
		if n, err := strconv.ParseInt(iv, 10, 64); err == nil {
			intentPtr = &n
		}
	}
	// Cursor precedence: the browser's automatic reconnect sends Last-Event-ID (the
	// last id it received) — trust it over the query so an auto-reconnect resumes
	// exactly where it dropped. A fresh/manual connect has no header and passes
	// since=<snapshot_cursor> from the history page instead.
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	if le := r.Header.Get("Last-Event-ID"); le != "" {
		if n, err := strconv.ParseInt(le, 10, 64); err == nil {
			since = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering

	// Subscribe BEFORE replaying history so events in between aren't lost; dedup the
	// overlap by skipping channel events whose id was already replayed.
	ch, unsub := s.engine.Broadcaster().Subscribe(t.ID)
	defer unsub()

	// Emit a standard SSE id: line so the browser echoes it as Last-Event-ID on
	// auto-reconnect (see cursor precedence above).
	sendSSE := func(a db.Activity) {
		b, _ := json.Marshal(activityDTO(a))
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", a.ID, b)
		flusher.Flush()
	}

	// Compensate the DB backlog after `since` in batches until caught up. This is the
	// gap between the history snapshot and the live tail — NOT the first-page history
	// (that's the /activity/history endpoint). A long task can have far more than one
	// batch, so loop instead of a single fixed read; on a query error log it and close
	// so the client reconnects and retries from its last id (broadcast is lossy — the
	// DB is the source of truth). The Broadcaster keeps buffering live events meanwhile;
	// the id<=since skip below drops any that this replay already covered.
	const replayBatch = 500
	for {
		items, cursor, err := t.Store.ActivityList(intentPtr, since, replayBatch)
		if err != nil {
			log.Printf("[activity/stream] task=%s replay since=%d: %v", t.ID, since, err)
			return
		}
		for _, a := range items {
			sendSSE(a)
		}
		if cursor > since {
			since = cursor
		}
		if len(items) < replayBatch {
			break
		}
	}
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case a, ok := <-ch:
			if !ok {
				return
			}
			if a.ID <= since {
				continue // already replayed
			}
			if intentPtr != nil && (a.NodeID == nil || *a.NodeID != *intentPtr) {
				continue // scoped session: only this intent's steps
			}
			since = a.ID
			sendSSE(a)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// activityDetail lazily returns the full detail blob for one step.
func (s *Server) activityDetail(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "no task")
		return
	}
	seq, _ := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	d, err := taskActivityDetail(t, seq)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"detail": d})
}

// taskActivityDetail keeps the legacy local-task behavior (including thinking
// rows), while inherited details are restricted to terminal worker intents.
// This prevents a guessed global activity id from exposing source planner/main
// or in-flight transcripts.
func taskActivityDetail(t *Task, seq int64) (string, error) {
	return t.Store.ActivityDetailWithSources(seq)
}

func (s *Server) getTraffic(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "exchanges": []any{}})
		return
	}
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 0)
	size := atoiDefault(q.Get("size"), 100)
	ex, matched, _ := tr.Page(traffic.PageQuery{
		Host:    q.Get("host"),
		Method:  q.Get("method"),
		Query:   q.Get("q"),
		Body:    q.Get("body"),
		Path:    q.Get("path"),
		Status:  q.Get("status"),
		RespMin: int64(atoiDefault(q.Get("resp_min"), -1)),
		RespMax: int64(atoiDefault(q.Get("resp_max"), -1)),
		Sort:    q.Get("sort"),
		Order:   q.Get("order"),
	}, page, size)
	count, _ := tr.Count() // global total, for the stat card
	writeJSON(w, 200, map[string]any{
		"enabled":   s.m.TrafficEnabled(), // reflect the capture toggle
		"proxy":     s.m.ProxyAddr(),
		"count":     count,   // total recorded (unfiltered)
		"total":     matched, // rows matching the current filter (for pagination)
		"page":      page,
		"size":      size,
		"exchanges": trafficDTOs(ex),
	})
}

// getTrafficHosts returns distinct recorded hosts with counts, for the page's
// target picker (pick a host → filter the list, then delete it).
func (s *Server) getTrafficHosts(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeJSON(w, 200, map[string]any{"hosts": []any{}})
		return
	}
	hosts, err := tr.Hosts()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"hosts": hosts})
}

// deleteTraffic removes recorded traffic for every host containing the query's
// host substring (the page's host filter is substring-based, so what you
// filtered is what gets deleted): index rows + each host's file tree, then
// garbage-collects blobs no remaining exchange references. Empty host → 400.
// Returns the number of exchanges deleted.
func (s *Server) deleteTraffic(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "traffic disabled")
		return
	}
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		writeErr(w, 400, "missing host")
		return
	}
	n, err := tr.DeleteHost(host)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// deleteTrafficHosts removes traffic for a set of EXACT hosts (JSON body
// {"hosts": [...]}) — the batch path for the page's multi-select delete. Exact
// match, so picking "api.example.com" never sweeps "api.example.com.cn".
// Returns the number of exchanges deleted.
func (s *Server) deleteTrafficHosts(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "traffic disabled")
		return
	}
	var req struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	hosts := make([]string, 0, len(req.Hosts))
	for _, h := range req.Hosts {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		writeErr(w, 400, "missing hosts")
		return
	}
	n, err := tr.DeleteHostsExact(hosts)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// deleteAllTraffic purges every recorded exchange, then compacts the index so
// the space is actually returned to the filesystem — an emptied index is the one
// moment a full rewrite is cheap. Evidence already bound to a finding lives in
// the evidence store and is deliberately left alone. Returns the number of
// exchanges deleted and the bytes of index reclaimed.
func (s *Server) deleteAllTraffic(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "traffic disabled")
		return
	}
	n, reclaimed, err := tr.DeleteAll()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n, "reclaimed": reclaimed})
}

// getTrafficExchange returns the full raw request/response of one exchange,
// read on demand from the traffic tree (bodies are not in the paged list).
func (s *Server) getTrafficExchange(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "traffic disabled")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, 400, "missing id")
		return
	}
	req, resp, err := tr.Get(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"req": req, "resp": resp})
}

// getTrafficBlob streams one oversized body by its sha256. Bodies past the inline
// threshold are not carried by the exchange endpoint — it returns a preview plus
// an "@blob sha256:<hash>" pointer — so this is how the UI fetches them whole.
// Streamed rather than buffered: these are the bodies too large to hold in memory.
func (s *Server) getTrafficBlob(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "traffic disabled")
		return
	}
	hash := strings.TrimSpace(r.URL.Query().Get("hash"))
	if hash == "" {
		writeErr(w, 400, "missing hash")
		return
	}
	f, size, err := tr.Blob(hash)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", hash+".bin"))
	if _, err := io.Copy(w, f); err != nil {
		log.Printf("[traffic] blob %s 다운로드 중단: %v", hash, err)
	}
}

// getSettings returns the runtime app settings the UI toggles. The Brave API key
// is returned as a boolean presence flag (brave_key_set), never the value itself,
// so the UI can show "configured" without echoing the secret back.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.settingsPayload())
}

func (s *Server) settingsPayload() map[string]any {
	on, backend, braveKey, tavilyKey, proxy := s.m.WebSearch()
	pyStored, _, _ := s.m.pg.GetSetting(settingPythonInterp)
	concOn, concLimit := s.m.ConcurrencyLimit()
	if concLimit == 0 {
		concLimit = defaultConcurrencyLimit // 꺼져 있어도 UI에 적절한 기본값을 보여 준다
	}
	return map[string]any{
		"traffic_capture":          s.m.TrafficEnabled(),
		"agent_traffic_binding":    s.m.pg.GetBool(settingAgentTrafficBinding, false),
		"llm_record":               s.m.LLMRecordEnabled(),
		"web_search_enabled":       on,
		"web_search_backend":       backend,
		"brave_key_set":            strings.TrimSpace(braveKey) != "",
		"tavily_key_set":           strings.TrimSpace(tavilyKey) != "",
		"web_search_proxy":         proxy,                       // 웹 검색 전용 아웃바운드 프록시(http/https/socks5). 비면 직접 연결
		"global_proxy":             s.m.GlobalProxy(),           // 전역 아웃바운드 프록시(http/https/socks5). 모든 대상 트래픽이 거친다. 비면 직접 연결
		"python_interpreter":       strings.TrimSpace(pyStored), // 사용자나 자동 감지가 정한 값(비면 실행 시 감지)
		"workers":                  s.m.Workers(),               // 동시 워커 에이전트 수(기본값 3). 이후 시작하는 작업에 적용
		"task_concurrency_enabled": concOn,                      // 작업 동시 실행 제한 켜기·끄기(기본값 꺼짐)
		"task_concurrency_limit":   concLimit,                   // 동시에 실행할 작업 최대 수(켜면 기본값 5)
		// LLM 장애 조치. 기본값 꺼짐. 켜면 전역 활성 프로필을 쓰는 에이전트가 현재 프로필을
		// 쓸 수 없을 때 다음 프로필로 자동 전환한다. bind_fallback은 장애 조치가 켜져 있을 때만 의미가 있다(기본값 꺼짐).
		"llm_pool_enabled":       s.m.LLMPoolEnabled(),
		"llm_pool_bind_fallback": s.m.LLMPoolBindFallback(),
		// 작업 제약 조건을 넣을 범위(기본값 모두 켜짐): 이 작업의 allow/deny 제약 조건을 해당 에이전트의 시스템 프롬프트에 넣는다.
		"constraints_inject_planner": s.constraintInjectPlanner(),
		"constraints_inject_worker":  s.constraintInjectWorker(),
		// 실험 기능: noa 모델 기반 컨텍스트 압축(기본값 꺼짐). 켜면 플랫폼에 연결된 네 종류의
		// 에이전트는 내장 compaction 대신 noa가 컨텍스트 압축을 맡는다. run마다 한 번 읽어 이후 시작하는 run에 적용된다.
		"noa_compaction": s.m.NoaCompactionEnabled(),
		// 취약점 IM 알림의 전역 항목. 알림 채널 자체는 독립 자원이라 /api/notify/*에서 관리하고,
		// 여기에는 '모든 채널에 적용되는' 세 항목만 둔다.
		"notify_enabled":             s.m.pg.GetBool(settingNotifyEnabled, true),
		"notify_public_base_url":     notifyPublicBaseURL(s.m.pg),
		"notify_digest_interval_min": notifyDigestIntervalMin(s.m.pg),
	}
}

// notifyPublicBaseURL 은 알림 상세 링크에 쓸 외부 링크 기본 주소를 읽는다.
func notifyPublicBaseURL(pg *db.DB) string {
	v, _, _ := pg.GetSetting(settingNotifyPublicBaseURL)
	return v
}

// notifyDigestIntervalMin 은 다이제스트 주기(분)를 읽고, 잘못됐거나 설정되지 않았으면 기본값을 쓴다.
// 빈 문자열 대신 기본값을 돌려줘야 UI가 현재 적용 중인 값을 입력 칸에 채울 수 있다.
func notifyDigestIntervalMin(pg *db.DB) int {
	v, ok, _ := pg.GetSetting(settingNotifyDigestMinutes)
	if !ok {
		return notifyDefaultDigestMinutes
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return notifyDefaultDigestMinutes
	}
	return n
}

// pgDetectPython re-runs interpreter detection, stores + returns it.
func (s *Server) pgDetectPython(w http.ResponseWriter, r *http.Request) {
	p := detectPython()
	if p == "" {
		writeErr(w, 404, "python을 찾지 못했습니다(python3·python 모두 PATH에 없습니다)")
		return
	}
	if err := s.m.pg.SetSetting(settingPythonInterp, p); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"python_interpreter": p})
}

// putSettings applies a settings change. Toggling traffic_capture rebuilds the
// agents (applyLLM) so the new proxy/traffic-tools/prompt state takes hold — when
// off, agents get no proxy config, no traffic tools, and no proxy prompt content.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TrafficCapture      *bool `json:"traffic_capture"`
		AgentTrafficBinding *bool `json:"agent_traffic_binding"`
		LLMRecord           *bool `json:"llm_record"` // LLM 기록 켜기·끄기(기본값 꺼짐). 바로 적용되며 에이전트를 다시 만들 필요가 없다
		// Web search. WebSearchEnabled/Backend toggle the tool + backend; BraveKey/TavilyKey
		// are optional — omit (null) to leave a stored key untouched, send "" to clear.
		WebSearchEnabled *bool   `json:"web_search_enabled"`
		WebSearchBackend *string `json:"web_search_backend"`
		BraveKey         *string `json:"brave_search_api_key"`
		TavilyKey        *string `json:"tavily_search_api_key"`
		WebSearchProxy   *string `json:"web_search_proxy"`   // 웹 검색 전용 아웃바운드 프록시(http/https/socks5). null이면 그대로, ""이면 비움
		GlobalProxy      *string `json:"global_proxy"`       // 전역 아웃바운드 프록시(http/https/socks5). null이면 그대로, ""이면 비움(직접 연결)
		PythonInterp     *string `json:"python_interpreter"` // 사용자 지정 스크립트 도구의 python 인터프리터 경로
		Workers          *int    `json:"workers"`            // 동시 워커 에이전트 수(>0). 이후 시작하는 작업에 적용
		// 작업 동시 실행 제한: 동시에 '실행 중'일 수 있는 작업 수. 끄면 제한 없음. 켜면 제한을 넘는 새 작업은 대기하고 자리가 나면 자동으로 시작한다.
		ConcurrencyEnabled *bool `json:"task_concurrency_enabled"`
		ConcurrencyLimit   *int  `json:"task_concurrency_limit"`
		// LLM 장애 조치 켜기·끄기와 '연결한 프로필이 실패해도 장애 조치 체인으로 대체' 켜기·끄기.
		// 둘 다 provider 체인을 다시 만들어야 적용되므로 아래 changed → applyLLM 경로로 간다.
		LLMPoolEnabled      *bool `json:"llm_pool_enabled"`
		LLMPoolBindFallback *bool `json:"llm_pool_bind_fallback"`
		// 작업 제약 조건을 넣을 범위 켜기·끄기(기본값 모두 켜짐). planner/worker가 회차마다 읽어 바로 적용되며 에이전트를 다시 만들 필요가 없다.
		ConstraintsInjectPlanner *bool `json:"constraints_inject_planner"`
		ConstraintsInjectWorker  *bool `json:"constraints_inject_worker"`
		// 실험 기능: noa 컨텍스트 압축 켜기·끄기(기본값 꺼짐). run마다 읽어 이후 시작하는 run에 적용되며 에이전트를 다시 만들 필요가 없다.
		NoaCompaction *bool `json:"noa_compaction"`
		// 취약점 IM 알림의 전역 항목. 셋 다 전달 엔진이 회차마다 한 번 읽으므로 바꾸면 바로 적용되고,
		// 에이전트를 다시 만들거나 재시작할 필요가 없다.
		NotifyEnabled    *bool   `json:"notify_enabled"`
		NotifyBaseURL    *string `json:"notify_public_base_url"`
		NotifyDigestMins *int    `json:"notify_digest_interval_min"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.ConstraintsInjectPlanner != nil {
		if err := s.m.pg.SetBool(settingConstraintsInjectPlanner, *req.ConstraintsInjectPlanner); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.ConstraintsInjectWorker != nil {
		if err := s.m.pg.SetBool(settingConstraintsInjectWorker, *req.ConstraintsInjectWorker); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.NoaCompaction != nil {
		// run마다 읽는 resolver라 바꾸면 이후 시작하는 run에 바로 적용되고 applyLLM으로 다시 만들 필요가 없다.
		if err := s.m.SetNoaCompaction(*req.NoaCompaction); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	// 알림 전역 항목: 전달 엔진이 회차마다 다시 읽으므로 바로 적용되고 재시작할 필요가 없다.
	if req.NotifyEnabled != nil {
		if err := s.m.pg.SetBool(settingNotifyEnabled, *req.NotifyEnabled); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.NotifyBaseURL != nil {
		// 끝의 슬래시를 늘 잘라 낸다: 상세 링크는 fmt.Sprintf("%s/function/...")로 붙이므로
		// 끝 슬래시가 남으면 "//function/..." 같은 이중 슬래시 경로가 된다.
		base := trimTrailingSlash(strings.TrimSpace(*req.NotifyBaseURL))
		if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			writeErr(w, 400, "링크 기본 주소는 http:// 또는 https://로 시작해야 합니다")
			return
		}
		if err := s.m.pg.SetSetting(settingNotifyPublicBaseURL, base); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.NotifyDigestMins != nil {
		// 최솟값은 1분이다. 더 짧은 주기는 실시간 발송과 같으니 그럴 때는 알림 채널을 realtime 모드로 바꾸면 된다.
		if *req.NotifyDigestMins < 1 || *req.NotifyDigestMins > 24*60 {
			writeErr(w, 400, "다이제스트 주기는 1~1440분이어야 합니다")
			return
		}
		if err := s.m.pg.SetSetting(settingNotifyDigestMinutes, strconv.Itoa(*req.NotifyDigestMins)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.Workers != nil {
		if err := s.m.SetWorkers(*req.Workers); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if req.ConcurrencyEnabled != nil || req.ConcurrencyLimit != nil {
		// 부분 PUT: 주지 않은 필드는 현재 값을 써서, 하나만 바꿀 때 다른 하나가 초기화되지 않게 한다.
		curOn, curLimit := s.m.ConcurrencyLimit()
		if curLimit == 0 {
			curLimit = defaultConcurrencyLimit
		}
		on, limit := curOn, curLimit
		if req.ConcurrencyEnabled != nil {
			on = *req.ConcurrencyEnabled
		}
		if req.ConcurrencyLimit != nil {
			limit = *req.ConcurrencyLimit
		}
		if err := s.m.SetConcurrency(on, limit); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		// 바로 한 번 조정한다: 끄면 대기 중인 작업을 모두 풀고, 제한을 올리면 빈자리만큼 시작한다. 다음 tick을 기다리지 않는다.
		go s.reconcileConcurrency()
	}
	if req.PythonInterp != nil {
		if err := s.m.pg.SetSetting(settingPythonInterp, strings.TrimSpace(*req.PythonInterp)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.LLMRecord != nil {
		// 기록기가 호출마다 이 플래그를 읽으므로 바꾸면 바로 적용되고 applyLLM으로 다시 만들 필요가 없다.
		if err := s.m.SetLLMRecordEnabled(*req.LLMRecord); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	changed := false
	if req.LLMPoolEnabled != nil {
		if err := s.m.SetLLMPoolEnabled(*req.LLMPoolEnabled); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = true // the provider chain itself changes shape → rebuild
	}
	if req.LLMPoolBindFallback != nil {
		if err := s.m.SetLLMPoolBindFallback(*req.LLMPoolBindFallback); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	if req.LLMPoolEnabled != nil || req.LLMPoolBindFallback != nil {
		// Pinned tasks' planner/worker and per-profile chat agents hold providers
		// built under the OLD switch state — drop them so they pick up the new one.
		s.invalidateProfileAgents()
	}
	if req.AgentTrafficBinding != nil {
		if err := s.m.pg.SetBool(settingAgentTrafficBinding, *req.AgentTrafficBinding); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.TrafficCapture != nil {
		if err := s.m.SetTrafficEnabled(*req.TrafficCapture); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	if req.GlobalProxy != nil {
		// Validation failure (bad scheme/host) is a client error, not a 500.
		if err := s.m.SetGlobalProxy(*req.GlobalProxy); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		changed = true // capture-off egress is baked into agents at build time → rebuild
	}
	if req.WebSearchEnabled != nil || req.WebSearchBackend != nil || req.BraveKey != nil || req.TavilyKey != nil || req.WebSearchProxy != nil {
		// Fill unspecified fields from current state so a partial PUT doesn't reset them.
		on, backend, _, _, _ := s.m.WebSearch()
		if req.WebSearchEnabled != nil {
			on = *req.WebSearchEnabled
		}
		if req.WebSearchBackend != nil {
			backend = *req.WebSearchBackend
		}
		if err := s.m.SetWebSearch(on, backend, req.BraveKey, req.TavilyKey, req.WebSearchProxy); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	if changed {
		// rebuild agents so the new proxy/tools/prompt/web-search take hold (only if LLM configured).
		s.cfgMu.Lock()
		cfg, on := s.llmCfg, s.llmOn
		s.cfgMu.Unlock()
		if on {
			if err := s.applyLLM(cfg); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
		}
	}
	writeJSON(w, 200, s.settingsPayload())
}

// testWebSearch runs a real "test" search with the given (or currently saved)
// backend/proxy/key to verify the config can actually reach a search backend —
// mirroring testLLM. Backend/proxy come from the request (so the form's unsaved
// edits are tested); empty API keys fall back to stored values so the user need
// not retype them. Always 200 with {ok, error?, count?, backend?}.
func (s *Server) testWebSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backend   string `json:"web_search_backend"`
		Proxy     string `json:"web_search_proxy"`
		BraveKey  string `json:"brave_search_api_key"`
		TavilyKey string `json:"tavily_search_api_key"`
	}
	// Empty body is fine — fall back entirely to the saved config below.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, 400, err.Error())
		return
	}
	_, backend, storedBraveKey, storedTavilyKey, _ := s.m.WebSearch()
	if strings.TrimSpace(req.Backend) != "" {
		backend = req.Backend
	}
	// Proxy is taken from the form as-is (empty = direct), so testing reflects exactly
	// what's shown — including an intentional "clear proxy to test direct" before saving.
	proxy := strings.TrimSpace(req.Proxy)
	// API keys are secrets the form omits when already saved, so fall back to stored.
	braveKey := storedBraveKey
	if strings.TrimSpace(req.BraveKey) != "" {
		braveKey = req.BraveKey
	}
	tavilyKey := storedTavilyKey
	if strings.TrimSpace(req.TavilyKey) != "" {
		tavilyKey = req.TavilyKey
	}
	cfg := actool.WebSearchConfig{Backend: backend, BraveAPIKey: braveKey, TavilyAPIKey: tavilyKey, Proxy: proxy}
	// Hard cap so a slow/blocked proxy can't hang the request.
	wall := 30 * time.Second
	// deepseek의 자격 증명은 폼이 아니라 현재 활성 LLM 프로필에서 온다. 또 검색마다 모델 추론을
	// 한 번 돌리므로 공통 상한 30s는 빠듯해 따로 늘린다. 프로필을 쓸 수 있는지 미리 판단하지 않는다:
	// 이 테스트가 바로 사용자가 직접 확인하는 수단이고, 실제로 안 될 때 아래 오류가 미리 판단한 것보다 정보가 많다.
	probeQuery := "test"
	if strings.TrimSpace(backend) == deepSeekWebSearchBackend {
		cfg.DeepSeekBaseURL, cfg.DeepSeekAPIKey, cfg.DeepSeekModel = s.m.deepSeekSearchCreds()
		wall = 120 * time.Second
		// 검색어는 DeepSeek 쪽 모델이 스스로 정한다. "test"는 너무 막연해 검색을 건너뛰고 바로 답해 버린다.
		probeQuery = "DeepSeek company official website"
	}
	ctx, cancel := context.WithTimeout(r.Context(), wall)
	defer cancel()
	results, err := actool.WebSearchProbe(ctx, cfg, probeQuery, 3)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "backend": backend})
		return
	}
	if len(results) == 0 {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "검색 결과가 0건입니다(속도 제한에 걸렸거나 프록시에 연결되지 않았을 수 있습니다)", "backend": backend})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(results), "backend": backend})
}

// mainSessions 는 작업의 메인 에이전트 대화 구간(최신순)과 현재 구간을 돌려준다. 프런트는 이를
// 메인 에이전트 아래에서 전환할 수 있는 세션으로 보여 준다.
func (s *Server) mainSessions(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	list, err := t.Store.ListMainSessions()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	current := 0
	if len(list) > 0 {
		current = list[0].Seq // newest-first
	}
	writeJSON(w, 200, map[string]any{"sessions": list, "current": current})
}

// newMainSession starts a fresh main-agent conversation segment. Only the segment
// counter advances — the task's exploration graph, assets and goal are untouched, so
// the main agent continues over the same task with a clean transcript/context.
func (s *Server) newMainSession(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	if s.engine.IsDeleting(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 새 세션을 만들 수 없습니다")
		return
	}
	m, err := t.Store.NewMainSession()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"seq": m.Seq, "created_at": rfc3339(m.CreatedAt), "current": m.Seq})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "no active task")
		return
	}
	if s.engine.IsDeleting(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 새 메시지를 보낼 수 없습니다")
		return
	}
	// 주의: 작업 일시 중지(paused)는 메인 에이전트 대화를 막지 않는다. 메인 에이전트 오케스트레이션
	// 세션은 planner/worker의 일시 중지와 별개라 일시 중지 중에도 대화할 수 있다(일시 중지는 진행 중인
	// 그 턴만 끝낸다. control() 참고).
	var req struct {
		Message     string           `json:"message"`
		Attachments []chatAttachment `json:"attachments,omitempty"` // 방식 1로 업로드한 파일(경로는 작업 작업 디렉터리 기준)
		Seg         *int             `json:"seg,omitempty"`         // 대상 메인 세션 구간. 생략하면 최신 구간
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	agentMessage, ok := s.prepareChatMentionMessage(w, req.Message)
	if !ok {
		return
	}
	// Admission is serialized with deletion's chat cancellation. Mark every chat
	// turn busy before its first activity/file write, including rule-mode turns.
	// If deletion wins the race, the second barrier check rejects this request.
	s.chatMu.Lock()
	if s.engine.IsDeleting(t.ID) {
		s.chatMu.Unlock()
		writeErr(w, 409, "작업을 삭제하는 중이라 새 메시지를 보낼 수 없습니다")
		return
	}
	if s.chatBusy[t.ID] {
		s.chatMu.Unlock()
		writeErr(w, 409, "메인 에이전트가 이전 메시지를 처리하는 중입니다. 잠시 뒤 다시 시도하세요.")
		return
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	ctx = intercept.WithReviewContext(ctx, "", intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: req.Message})
	s.chatBusy[t.ID] = true
	s.chatCancel[t.ID] = cancel
	s.chatMu.Unlock()

	// The turn belongs to whichever main-agent segment the user is chatting in (any
	// segment is interactive, like the top-level chat conversations). Stamp every
	// mainagent row with it so this turn's transcript + activity land in that segment.
	// Missing seg (older clients) falls back to the newest segment.
	mainSeg := 0
	if req.Seg != nil && *req.Seg >= 0 {
		mainSeg = *req.Seg
	} else {
		mainSeg, _ = t.Store.CurrentMainSeg()
	}
	segPtr := &mainSeg

	// 사람 차례를 저장하고 방송해 메인 에이전트 오케스트레이션 세션이 새로 고침 뒤에도 남고
	// 실시간으로 갱신되게 한다: 대화는 활동 스트림에 worker="mainagent"로 들어간다(작업별 활동
	// 표, SSE로 다시 재생된다). 첨부 파일이 있으면 활동의 Detail이 {text, attachments}를 담아
	// 대화 기록에 첨부 파일 카드가 보인다.
	humanTurn := userActivityWithAttachments("mainagent", req.Message, req.Attachments)
	humanTurn.MainSeg = segPtr
	s.engine.emitActivity(t, humanTurn)
	var ma *agent.MainAgent
	if s.taskRuntimeAvailable(t, "mainagent") {
		ma = s.agentsForTask(t).main
	}
	if ma != nil {
		// Run the agent on a per-turn cancellable ctx (NOT r.Context()): the turn can
		// take minutes (multi-tool loop), and binding it to the request lifecycle meant
		// a page reload / proxy timeout cancelled it mid-run ("context canceled"). The
		// steps + final answer stream back live via SSE (worker="mainagent"), so the
		// handler returns immediately and the browser never needs to hold the request.
		go func() {
			defer func() {
				s.finishTaskChat(t.ID, cancel)
			}()
			// emit every step (thinking/tool_use/tool_result/text/result) so the main
			// agent session shows its work live, like worker/planner. The final answer
			// is the captured "result" step — no separate reply emit (would duplicate).
			emit := func(rec db.Activity) {
				rec.MainSeg = segPtr
				s.engine.emitActivity(t, rec)
			}
			maTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
			resume := func() { s.reviveTask(t) } // set_goals가 목표를 추가하면 작업을 running으로 되돌린다
			// 업로드한 첨부 파일의 절대 경로 목록을 에이전트에 보내는 메시지에 붙여, 에이전트가 Read/Bash로 열게 한다.
			// taskDir = 에이전트의 작업 디렉터리(CWD). chatUpload가 저장하는 곳, ensureRunDir와 같다.
			taskDir := filepath.Join(s.m.dir, "tasks", t.ID)
			agentMsg := composeAgentMessage(agentMessage, req.Attachments, taskDir)
			s.engine.BeginLLMCall(t.ID)
			_, err := ma.Chat(ctx, maTaskID, mainSeg, s.m.Assets(), t.Store, t.Goal, agentMsg, emit, t.Notify, resume, t.NotifyGoal, t.NotifyHint)
			s.engine.EndLLMCall(t.ID)
			if err != nil && ctx.Err() == nil {
				s.engine.emitActivity(t, db.Activity{Worker: "mainagent", Kind: "text", IsError: true, Summary: "(메인 에이전트 오류: " + err.Error() + ")", MainSeg: segPtr})
			}
		}()
		writeJSON(w, 202, map[string]any{"status": "accepted", "mode": "llm"})
		return
	}
	reply := s.fallbackChat(t, req.Message)
	s.engine.emitActivity(t, db.Activity{Worker: "mainagent", Kind: "text", Summary: reply, MainSeg: segPtr})
	s.finishTaskChat(t.ID, cancel)
	writeJSON(w, 200, map[string]any{"reply": reply, "mode": "rule"})
}

func (s *Server) cancelTaskChat(taskID string, cause error) bool {
	s.chatMu.Lock()
	cancel := s.chatCancel[taskID]
	if cancel != nil {
		cancel(cause)
	}
	s.chatMu.Unlock()
	return cancel != nil
}

func (s *Server) finishTaskChat(taskID string, cancel context.CancelCauseFunc) {
	cancel(agent.AbortChatTurnFinished)
	s.chatMu.Lock()
	delete(s.chatBusy, taskID)
	delete(s.chatCancel, taskID)
	s.chatMu.Unlock()
}

// taskChatStatus reports the authoritative state of the task's main-agent turn.
// Activity timestamps are not a reliable proxy because a tool or LLM call may run
// for minutes without emitting an intermediate frame.
func (s *Server) taskChatStatus(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	s.chatMu.Lock()
	running := s.chatBusy[t.ID]
	s.chatMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"running": running})
}

// stopChat aborts the in-flight main-agent turn for a task (manual stop button).
// Mirrors pgStopConversation: cancels only the current turn; planner/workers are
// unaffected and continue running. The user can send a new message immediately.
func (s *Server) stopChat(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.cancelTaskChat(t.ID, agent.AbortChatStoppedByUser) {
		writeJSON(w, 200, map[string]any{"status": "idle"})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "stopping"})
}

// fallbackChat 은 LLM 없이 사람이 조종하는 처리기다: 간단한 명령 + 현황 요약.
// 아래 중국어 접두사는 기존 사용자 입력과의 호환을 위해 영어 접두사와 함께 그대로 받는다.
func (s *Server) fallbackChat(t *Task, msg string) string {
	m := strings.TrimSpace(msg)
	lower := strings.ToLower(m)
	switch {
	case strings.HasPrefix(m, "意图") || strings.HasPrefix(lower, "intent"):
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(m, "意图"), "intent"))
		_, _ = t.Store.AddIntent(map[string]any{"summary": text}, 9, nil, "human")
		return "우선순위가 높은 의도 하나를 넣었습니다: " + text
	case strings.HasPrefix(m, "提示") || strings.HasPrefix(lower, "hint"):
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(m, "提示"), "hint"))
		_, _ = t.Store.AddNode(db.KindHint, map[string]any{"text": text}, 0, "active", "human", nil)
		return "힌트를 기록했습니다. 플래너가 다음 회차에 읽습니다: " + text
	default:
		assetCounts, _ := s.m.Assets().CountsByType()
		assets := 0
		for _, c := range assetCounts {
			assets += c
		}
		fnd, _ := t.Store.ListByKind(db.KindFinding, 1000)
		fr, _ := t.Store.Frontier(1000)
		return fmt.Sprintf("(규칙 모드, LLM 설정 안 됨) 현재 상황: 자산 %d, 할당 대기 의도 %d, 확인된 발견 사항 %d.\n쓸 수 있는 명령: \"intent ...\"로 의도를 넣고, \"hint ...\"로 플래너에 힌트를 줍니다.", assets, len(fr), len(fnd))
	}
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "no active task")
		return
	}
	findings, _ := t.Store.ListByKind(db.KindFinding, 1000) // 취약점만(사실은 별도 KindFact라 보고서에 넣지 않는다)
	counts := map[string]int{}
	for _, ty := range []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"} {
		ns, _ := s.m.Assets().QueryByType(ty, 100000, 0)
		if len(ns) > 0 {
			counts[ty] = len(ns)
		}
	}
	md := report.Markdown(report.Input{
		Title: t.Description, Goal: t.Goal, GeneratedAt: time.Now(),
		AssetCounts: counts, Findings: findings,
	})
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(md))
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, []any{})
		return
	}
	writeJSON(w, 200, map[string]any{"entries": t.Guard.Audit(), "attributions": t.Guard.Attributions()})
}

// gc is a no-op stub (GC not yet implemented in the new asset store).
func (s *Server) gc(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"removed": 0})
}

// --- utils ---

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func atoiDefault(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}
