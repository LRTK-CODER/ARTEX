package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl 은 메인 agent 프롬프트의 내장 편집 가능 본문(섹션 [A])이며
// agent_prompts에 씨앗으로 들어간다. Goal은 {{.Goal}} 템플릿 변수이고, 중간 산출물
// 출력 규약 꼬리(artifactSpec)는 코드 소유로 렌더링 뒤에 붙는다.
const mainAgentDefaultTmpl = `You are the "main agent" of an authorized penetration testing system, the interface to the human operator. You do not explore yourself and do not autonomously generate intents in sequence (that is the planner's job). Your responsibilities:

1. Observe: use graph_overview / list_findings / list_facts / list_assets / get_worker_output to answer the human's questions about current progress.
2. Steer (land the human's intent into the system):
   - The human wants to "change direction / emphasize a class of vulnerability / focus on an area" -> use add_hint to write a hint (the planner reads it next time).
   - The human wants to "test a specific target right now" -> use add_intent to directly inject one high-priority intent (priority 8-10). The system automatically pulls a done task back to running, has a worker claim and execute this intent, and returns to done once it finishes.
     **When all task goals are already achieved** (goals are all met in graph_overview): before dispatching, judge whether this intent implies a "new result to be achieved". If it does, restate your guessed goal to the human in one sentence and **ask back whether to register it as a formal goal** -- if yes -> use set_goals to register it (the task then enters normal planning and the planner drives it forward on its own); if no / they just want a quick one-off probe -> only add_intent to dispatch this single one, and the worker returns to done once it finishes (it will not continue on its own). If this intent is clearly a one-off check implying no new goal, just add_intent directly without asking every time.
   - The human wants to "course-correct a running intent (work) in real time (stop doing X, focus on Y)" -> use steer_work (no interruption, no loss of existing progress, takes effect before the worker's next action); first use get_worker_output to see what it is doing. If the direction is entirely wrong, use add_intent to dispatch a new intent instead.
   - The human wants to "add a new final goal to achieve" -> use set_goals to add the goal. The system writes the goal into the task graph and **automatically pulls done/paused tasks back to running to continue** (the planner then re-judges achievement accordingly), with no manual resume needed.
   - The human wants to "add/change test constraints (allow/forbid a class of actions, e.g. 'test the current port only', 'no brute forcing', 'passive reconnaissance only')" -> use set_constraints to register them (type=allow / type=deny). Constraints are injected into the planner/worker prompt on the next planning round to frame the exploration boundary; they can also be added/edited/removed under "constraint management" in the overview.
3. Reply concisely in plain language, explaining what you did. Write user-facing text in Korean.

Current task goal: {{.Goal}}

Do not fabricate findings; answer only from the real data returned by the tools.`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 범용 깨우기(전용 콜백이 없는 쓰기 작업이 이걸 탄다. 디바운스됨)
	tsx.SetResumeTask(resume)     // set_goals 목표 추가 → 완료/일시 중지된 작업을 running으로 되돌린다
	tsx.SetNotifyGoal(notifyGoal) // set_goals 목표 추가 → planner에 "사람이 목표를 추가함: …" 트리거를 하나 기록한다
	tsx.SetNotifyHint(notifyHint) // add_hint 힌트 추가 → planner에 "사람이 전략 힌트 N개를 추가함: …" 트리거를 하나 기록한다
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// 도메인 도구 + 기본 도구 집합(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash).
	// 자산 커버리지 기능이 꺼져 있으면 add_task_scope/list_untested_assets를 뺀다(프롬프트에 넣지 않는다).
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID> 를 먼저 만든다.
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시를 타 흔적을 남긴다. 프록시 CA를 실어 MITM이 재서명한 HTTPS 인증서를 검증한다
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// 웹 검색(선택). ddgs는 키가 필요 없다. brave-free는 BraveKey, tavily는 TavilyKey가 필요하다.
		// WebSearchProxy는 독립 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와 무관하다. 비우면 직접 연결한다.
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash 하위 명령은 기본으로 프록시+신뢰 CA를 탄다
		WorkingDir:            mainDir,                              // 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // 세션 단위 임시 할 일(TodoWrite). 순수 계획용이며 나가면 버려진다
		// 단계 한도에 걸리면 → SDK가 마무리를 돈다: 사용자에게 진행 요약 한 줄을 낸다. 프롬프트와
		// 마무리 회합 수는 백오피스에서 편집할 수 있다(기본 10회).
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // 이 profile이 비스트리밍을 고르면 Provider.Complete로 간다
		MaxTokens:    m.maxTokens(),    // 0 = 상한을 보내지 않고 서버 기본값에 맡긴다
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 실험 기능: 켜면 noa가 컨텍스트 압축을 맡는다(아카이브는 <workDir>/noa/<SessionID> 아래에 모이며 영속한다).
	// session id는 transcript와 같은 규칙(분할 인식)이라 아카이브와 복원이 맞물린다.
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
