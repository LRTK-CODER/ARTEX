package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// deepseek 백엔드는 나머지 셋과 성질이 다르다: DeepSeek 에는 바로 호출할 수 있는 검색 API 가
// 없고, 검색은 그 Anthropic 호환 messages API 안에만 있다(web_search_20250305 server tool).
// 그래서 검색 한 번이 모델 호출 한 번을 쓰고, 검색 요청은 DeepSeek 서버가 보낸다 — 로컬 Proxy 를
// 거치지 않고 트래픽에도 남지 않는다.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* 는 현재 활성 LLM 설정에서 온다(anthropic 형식의 DeepSeek 공식 엔드포인트만).
	// 따로 설정하지 않고 LLM 설정이 바뀌면 함께 바뀐다.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "You are about to be terminated because the budget is exhausted. Do not run any more commands/probes. In order: (1) write back, one by one, everything you have already identified above but not yet persisted -- new assets with insert_assets, exploration conclusions/facts with record_fact, confirmed vulnerabilities with report_finding; (2) **finally, in a single plain-text sentence**, summarize what you did and the key conclusions you reached (this sentence is displayed as this run's result, so be sure to output it)."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl 은 worker 시스템 프롬프트의 내장 편집 가능 본문(섹션 [A])이며 agent_prompts 에
// 씨앗으로 들어간다. trafficTool 블록과 중간 산출물 출력 규약은 여기에 없다 — 코드 소유라
// workerSystem 이 렌더링 뒤에 붙인다(섹션 [B]/[C]). 그래서 DB 본문을 편집해도 그 둘을 떼어낼 수 없다.
const workerDefaultTmpl = `You are the "executor" (work agent) of an authorized penetration testing system on a cybersecurity platform. You are assigned [one intent] (a one-sentence exploration direction); your sole responsibility: **complete this one intent, write the findings back into the knowledge graph, then stop and return.**

**Boundaries (red lines)**:
1. **Do only the one intent you were assigned.** **If, while exploring this intent, you glimpse a lead worth digging into beyond this intent** (a path leaked by an error, a point that may link with other assets, a suspected entry to another exploit chain), **note it in one sentence in the fact's summary and hand it to the planner.**
2. An initial setback (a payload filtered / 404 / injection with no echo-back) does not mean you have exhausted it -- run through all the bypass techniques for this intent before producing a conclusion;
3. Operate only within the authorized scope. If [operation constraints] are attached at the top of the system prompt, they are the highest-priority red line: self-check before executing each command/probe, and if it violates them do not do it (even if it falls within the intent you were assigned).

**Write back as you discover** (only what is written into the graph counts; what is in your head or text does not; write each result immediately, do not pile them up for the end and lose them when steps run out). Three kinds of write-back; do not cross the graphs:
- **New asset/resource -> insert_assets (asset graph)**: subdomain / service / endpoint / fingerprint / credential and every asset **itself**. **Register only assets here; do not write exploration conclusions/judgments here, use record_fact.**
- **Exploration conclusion/fact -> record_fact (exploration graph, pass intent_id)**: use it for all of them. **Aggregate multiple observations into [one] fact** (a one-sentence summary + detail that correctly expands on the summary, grounded in the real execution process); do not make one per attribute -- usually only one per intent, since fragmenting bloats the graph without bound -- **by default write one, and merge into detail everything you can**; use the facts array to split into entries only when there are genuinely [fully independent, unmergeable] conclusions, which is a rare exception, not the norm. **Write only increments**: record only what you **newly obtained** this time, do not re-record an existing fact with different wording (if you are only corroborating an existing one with nothing new, there is no need to record). **Write only what you actually saw**: give evidence (one line: the command + the one or two output lines that best prove it, concise, with details in detail) and mark confidence (observed=seen directly / inferred=inferred from a phenomenon).
- **Confirmed vulnerability -> report_finding (exploration graph, with PoC, pass intent_id)**: **use it only when you actually triggered it this run and obtained reproducible evidence (request/response or command output)**. Never treat "version/fingerprint matched a CVE", "the parameter looks injectable", or "inferred from an external vulnerability database/changelog/code diff" as confirmed, and do not substitute querying a CVE database or comparing patch versions for actually triggering it. If you cannot trigger it but suspect it -> record an inferred fact with record_fact (the suspected point + why it was not triggered) and hand it to the planner rather than forcing it into a finding.


After completing this intent, summarize in one sentence what you did and which facts you wrote back.`

// workerTrafficBlock is 섹션 [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**Traffic tools**:\n- traffic_search / traffic_get / traffic_blob: review responses and find already-visited resources; **search traffic first, do not curl the same URL again**. traffic_search **must specify host** and by default returns only 3 very lightweight index entries (id/method/url/status/resp_len, with no response body); raise limit explicitly for more; use body_contains for full-text search over the request/response body (at least 3 characters, supports substrings and Chinese, e.g. to find a password/key/error/internal address); to see the original of an entry use traffic_get(id), where an oversized body is shown as @blob sha256:<hash>, and use traffic_blob(hash) to fetch the full text in segments."
}

// artifactSpec is 섹션 [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**Intermediate-artifact output convention**: scripts, payloads, captured response bodies, temporary data, and all other intermediate artifacts **must be written to this task's working directory " + dir + "** (a relative path lands here, or use this absolute path) -- **do not write to /tmp and do not use any other absolute path**."
}

// workerArtifactSpec is the worker's 섹션 [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**Intermediate-artifact output convention**: scripts, payloads, captured response bodies, temporary data, and all other intermediate artifacts **must be written to this intent's dedicated working directory " + runDir + "** (already created, just write relative paths here, no need to mkdir manually) -- **do not write to /tmp and do not use any other absolute path**."
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n**The intent you were assigned (your only task this time: do only this one, produce only facts, and stop when done)**:\n%s\nintent id: %d (pass it when writing back with record_fact / report_finding)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage 는 규획자가 "어떤 유형이 덜 테스트됐나 / 범위를 넓힐까"를 판단하는 신호로,
	// "받은 의도만 하고 미커버 지점을 쫓지 말라"는 worker 의 직무 경계와 어긋난다 → worker 뷰에서 뺀다.
	// data 는 이번 worker 전용의 새 map 이라 키를 지워도 planner 에 영향이 없다.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n**Global exploration situation (read-only, to help you place your own intent in the big picture)**:\n" +
		"Below is the whole task's current exploration overview. It has two uses: one, to know what others have already found so you do not duplicate; two, so that while exploring your own intent you can associate it with the global picture.\n" +
		"**Divergent thinking is good**: while exploring this intent, think deep and associate freely. The only line is this -- do not actually go execute another intent (that is another worker's job, scheduled by the planner). Whenever you associate a valuable lead (a cross-asset linkage, a suspected entry to another exploit chain, a globally suspicious point), **be sure to write it into a fact and hand it to the planner** -- this is an important part of your output, not optional. Better to report one extra for the planner to judge than to swallow it yourself.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding 가 저장될 때 그 자리에서 planner 를 깨운다. "어느 의도 + finding"을 함께 전한다
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// worker 에는 일부러 MultiEdit/Glob/Grep 을 주지 않는다: 파일 정밀 수정은 Edit 로, 검색은
	// Bash(grep/find)로 한다. 도구 면을 좁혀 가치 낮은 호출을 줄인다. 나머지 SDK 기본 도구
	// (Read/Write/Edit/LS/Bash/Sleep)는 그대로 둔다.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// 의도는 worker 의 [유일한 직무이자 run 전체를 관통하는 불변식]이다 → 시작 지시, 의도가 지정한
	// 목표 자산 원본 데이터와 함께 system prompt 에 넣는다: system 은 run 마다 다시 조립되어 절대
	// compaction 에 눌리지 않으므로, 긴 run 에서도 의도가 늘 있고, 이어서 돌 때도 transcript 역사가
	// 그 첫 메시지를 보존했는지에 기대지 않는다. 대가는 system 에 의도별 가변 데이터가 섞여 의도 간
	// 캐시 재사용을 잃는 것이다. 의도적인 맞바꿈이다(의도를 잃는 쪽이 토큰을 아끼는 것보다 훨씬 심각하다).
	// planner 가 "태세 블록을 user turn 에 둔 것"과 갈라지는 것도 의도적이다: planner 는 의도를 만드는
	// 쪽이라 단일 mandate 가 없고, worker 는 있다. [전역 태세 overview]만 시작 user 메시지에 남긴다 —
	// 그것은 격하 가능하고 stale 를 견디며 눌려도 무방하다.
	// 이번 의도의 전용 작업 디렉터리 <workDir>/tasks/<taskID>/i<intentID> 는 엔진 쪽에서 먼저 만든다.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 작업 제약 조건(있으면)을 시스템 프롬프트에 주입한다. worker 는 실행 때 엄격히 지킨다
	}
	// 의도 블록 → 의도가 지정한 자산 블록 → 시작 지시 순으로 system 끝에 이어 붙인다(constraintBlock 과 같은 방식).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\nTarget assets corresponding to this intent's asset_ids:\n" + string(b)
				}
				// 이 의도가 분명히 겨냥하는 자산들 → 작업 테스트 범위에 자동 편입한다(insertAssets 와 같은
				// 보수적 세분화 수준). upsertTaskScope 의 ON CONFLICT DO NOTHING + uq_task_scope 유니크 인덱스가
				// 중복 추가를 막는다. 재실행/재시도도 똑같이 멱등 no-op 이다.
				// 자산 커버리지 기능이 꺼져 있으면 테스트 범위(분모)를 더 쌓지 않는다.
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\nBegin executing the intent above: do only it, produce only facts, assets, and findings, and stop when done."
	system, boundary := deferredSystem(sysBody, def)
	// 작업 단위 deadline(ctx 로 주입)이 이 run 의 벽시계 예산을 좁히고 마무리 문구를 정한다(taskclock.go 참고).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch 는 기록 프록시를 타므로 그 HTTP 가 curl 과 똑같이 흔적으로 남는다. 프록시 CA 를
		// 실어 MITM 이 재서명한 HTTPS 인증서가 [정상적으로 검증을 통과]하게 한다(검증을 끄는 게 아니다). 프록시가 비면 직접 연결한다.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// 웹 검색(선택). ddgs 는 키가 필요 없다. brave-free 는 BraveKey, tavily 는 TavilyKey 가 필요하다.
		// WebSearchProxy 는 독립 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와 무관하다. 비우면 직접 연결한다.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash 하위 명령의 HTTP 는 기본으로 기록 프록시를 타고 그 CA 를 신뢰한다(도구에 -x/-k 가 필요 없다).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 벽시계 예산. 회합 경계에서 판정하며 중간에 끊지 않는다. 0 = 제한 없음. 작업 단위 deadline 이 있으면
		// min(자체 예산, deadline 까지 남은 시간)으로 좁혀, 작업이 시간에 다다랐을 때 이 run 이 자연히 마무리로 들어가게 한다(taskclock.go 참고).
		MaxDuration: maxDur,
		// 예산(회합 또는 시간)에 걸리면 → SDK 가 마무리 라운드를 한 번 돈다(Bash 를 숨김). 이미 식별한 것을 써 넣어 어중간한 끝을 막는다.
		// clamped(작업 deadline 에 좁혀짐)일 때는 PromptByReason 을 쓴다: 시간 초과=작업 시간에 다다름→작업 시간 초과 문구,
		// 단계=좁혀진 창 안에서 단계가 먼저 소진→per-run 문구로 되돌린다. 비 clamped 면 순수 per-run 을 유지한다.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. 잘림 상한은 SDK 기본값(30000자)을 쓴다.
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // 세션 단위 임시 할 일(TodoWrite). 순수 계획용이며 나가면 버려진다
		NonStreaming:  w.nonStreaming(),                       // 이 profile 이 비스트리밍을 고르면 Provider.Complete 로 간다
		MaxTokens:     w.maxTokens(),                          // 0 = 상한을 보내지 않고 서버 기본값에 맡긴다
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 의도 / 시작 지시 / 의도가 지정한 자산은 이미 system prompt 로 내려갔다(위 sysBody 조립 참고).
	// 이 시작 user 메시지는 [전역 태세 overview]만 나른다 — 격하 가능한, 대국을 파악하는 정보라 눌려도 무방하다.
	// overview 가 드물게 marshal 실패로 비면, 첫 라운드에 빈 user 메시지가 나오지 않도록 시작 문구로 되돌린다.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "Begin executing the intent assigned in the system prompt: do only it, produce only facts, assets, and findings, and stop when done."
	}

	// 실험 기능: 켜면 noa 가 컨텍스트 압축을 맡는다(아카이브는 <workDir>/noa/<SessionID> 아래에 모이며 영속한다).
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "Continue."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "Continue executing the new intent from the previous human conversation input. Do not repeat actions already completed."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n**[New intent from human conversation input]**\n" + message +
				"\n\nExecute this human input immediately; after finishing, decide from the context whether the original task needs to continue."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n**[New intent from human conversation input]**\n" + message +
				"\n\nPrioritize executing this human input."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
