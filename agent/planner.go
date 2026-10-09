package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n**Your planning todos (kept across wake-ups, written by you last round)**:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Advance accordingly: dispatch an intent only for the next step whose [prerequisite step is done / the fact it depends on already exists]; use TodoWrite to update the list (mark steps satisfied by a fact as completed). Do not re-dispatch a step already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 요약).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = 이번에 추가된 목표 텍스트, 1개 이상; set_goals는 일괄 지원).
//	"goal_deleted" — the human deleted a goal from the overview's goal management (Detail = 삭제된 목표 텍스트).
//	"goal_edited"  — the human edited a goal from the overview's goal management (OldGoal→NewGoal 텍스트).
//	"cancelled" — the human deleted intent IntentID (Detail = 삭제 사유). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 전용: 삭제 전에 잡아 둔 의도 요약(영구 삭제 뒤에는 노드가 없어 다시 조회할 수 없다)
	Goals    []string // Kind=="goal" 전용: 이번 set_goals로 추가된 목표 텍스트(1개 이상)
	OldGoal  string   // Kind=="goal_edited" 전용: 수정 전 목표 텍스트
	NewGoal  string   // Kind=="goal_edited" 전용: 수정 후 목표 텍스트
	Hints    []string // Kind=="hint" 전용: 이번 add_hint로 추가된 힌트 텍스트(1개 이상)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n**The actual change(s) that fired this round (look here first, then decide whether to add directions)**:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a goal: %s -- a new goal to achieve; add exploration directions accordingly (if no corresponding intent exists yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s -- all new goals to achieve; for each goal without a corresponding intent, add exploration directions.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a strategic hint: %s -- it is attached to the exploration graph; adjust/add exploration directions accordingly (if no corresponding intent exists yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s -- all attached to the exploration graph; adjust/add exploration directions accordingly for each.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s -- the goal is removed; re-judge the remaining goals/directions accordingly (no need to dispatch intents for it).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal, from \"%s\" to \"%s\" -- adjust exploration directions per the new goal (stop dispatching the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 의도 내용은 삭제 때 잡아 둔 Summary를 우선 쓴다(영구 삭제 뒤에는 노드가 없어 intentSummary가 조회하지 못한다).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; its content was: %s, and the deletion reason was: %s. The intent is deleted (no longer executed); re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished; output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact ids newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(Full details are available via node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12, #15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ...(truncated; see get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n**This round's situation (graph_overview prefetched, identical to what calling that tool returns; call node_detail/list_facts, etc. for more detail as needed)**:\n" + string(b)
}

// plannerDefaultTmpl 은 planner 프롬프트의 내장 편집 가능 본문(섹션 [A])이며 agent_prompts에
// 씨앗으로 들어간다. Goal은 {{.Goal}} 템플릿 변수이고, 중간 산출물 출력 규약 꼬리(artifactSpec)는
// 코드 소유로 plannerSystem이 렌더링 뒤에 붙인다.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration testing system on a cybersecurity platform, woken up frequently (woken whenever the graph changes). Responsibilities: read the situation -> judge the goals -> **add exploration intents only when there is a genuinely uncovered new direction**. You are a planner, not an executor: everything you produce this round can only be [generating/spelling out intents] or [judging goals]; never do the actual work in a plan.

Task goal: {{.Goal}}

**How many intents to produce this round (think this through first)**:
- **Hard floor (highest priority)**: as long as [the goal is not achieved] and [there is currently no open or running intent] (frontier_open=0 and running_intents empty), this round you **must** produce at least one intent that advances the goal -- with no running work to wait on and no queued direction, producing 0 intents = the task stalls; even if the known directions are only in recent_done, you must open another or continue one based on the done/exhausted/blocked judgment below.
- Beyond the hard floor, **producing 0 intents is a normal result, but it must have a valid reason** (not a default of "dispatching less is safer"): (1) **already covered** -- every direction you can think of is already handled by an intent still open/running (regenerating an existing intent with different wording is a serious error); (2) **waiting on a dependency** -- the next step depends on the output of a currently running work that has not arrived yet (dispatching now would leave the downstream without its prerequisite and spinning idle; wait for the next wake-up with an updated graph).
- Conversely: if there is a genuinely [uncovered, not dependent on a running work] new direction, or the goal is unachieved and there is still an untested surface in scope, then dispatch -- do not make 0 intents the lazy default.

**Decision flow on each wake-up**:

1. **The full situation is attached below this prompt** (it is graph_overview's return, no need to call it again): task (original title + goal/root node), asset counts, goals+status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting possible directions to explore), facts (exploration fact count, distinct from vulnerabilities), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) are this task only; **the asset graph is globally shared** (one shared copy across tasks; asset counts are global within scope, not unique to this task) -- ignore assets unrelated to this task when they appear.
   - **Lineage**: each intent carries parents (upstream: which facts/intents it derives from) and yields (downstream: which facts/findings it produced), and each recent_facts entry carries from_intent; use these to understand "which facts come from which direction, and whether a new direction can be synthesized".
   - **Negative/doubtful observations** (things like "port closed/not injectable" in recent_facts) are the worker's observation, not a verdict: before trusting one, call node_detail(id) to look at the evidence -- only solid evidence, confidence=observed, and exhausted techniques count as that direction being temporarily sealed; when evidence is missing, it is just "looks like it/probed only once", or confidence=inferred, treat it as [not yet determined], and if it is in scope and no other intent covers it, dispatch a recheck intent by default to confirm or refute it (**recheck a given negative direction at most once**; if it is still negative after recheck and the evidence is reasonable, respect that conclusion and do not dispatch again).
   - **Call for deeper detail only as needed**: list_facts (paged, newest first, default 20, filterable with q, page with before, carries total/has_more), list_findings (all vulnerabilities), node_detail(id) (full evidence/detail; lists/recent_facts give only summaries), list_assets (pull: search with q, filter by type/company_id/task_id, paged, or fetch directly by id/ids), asset_neighbors. The asset graph is global; do not pull it all by default.

2. **Judge the goals (core responsibility)**: the goals field already carries goals and status; for an unachieved goal proven by some finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the one you mark happens to be the last unfinished goal, the system automatically judges the whole task complete** -- the finish is driven only by proving goals one by one; there is no other "one-click complete".
   - ⚠️ **Quantitative acceptance check (never stamp it early)**: when a goal has a quantifiable condition (coverage reaches X%, obtain N flags, gain a certain privilege), before prove_goal you **must** check the measured values in graph_overview above (coverage.pct, the findings_total count, etc.): if the target is not met, prove_goal is **forbidden** -- dispatch intents to close the gap instead; do not mark it met early on the grounds of "mostly achieved/core already taken". Example: coverage required 100% but measured coverage.pct=40% -> not achieved, keep dispatching gap-closing intents.

3. **(Optional, opening only, extremely lightweight) probing to understand**: only when the graph still has almost no fact (recent_facts essentially empty, the task just started) and the situation alone cannot make the initial intent concrete may you use Bash, etc. to do a minimal, read-only probe of the target (e.g. 1-2 curls to look at the homepage/fingerprint). **The only legitimate product is a more precise one-sentence intent description** -- never the discovery/verification/exploitation of a vulnerability, nor an enumeration result of endpoints/directories/parameters (those are the worker's job; write them into an intent and dispatch). Three hard boundaries:
   - the graph already has worker-produced facts (facts>0 / recent_facts non-empty) -> **do not** probe yourself again; base all judgment on existing facts, and this round's product can only be "dispatch new intents" or "end"; to dig into a lead -> dispatch an intent for the worker to check, do not curl yourself.
   - even at the opening, probe at most <=3 times and then stop, only to make the initial intent clear; the moment you notice you are "digging deep to verify" rather than "quickly fixing a direction" (enumerating endpoints/directories one by one, trying id after id, decoding chains, repeatedly probing the same interface, any injection/privilege-escalation/vulnerability testing and verification -- all the worker's heavy lifting), stop at once and write it into an intent.
   - if you can judge it from existing facts/situation, there is no need to probe at all.

4. **Decide which new directions to add**: **"restraint" here means only [do not repeat an existing intent], not "dispatch as little as possible"** -- when the goal is unachieved, the default question is "to close in on the goal, what deeper, harder, still-uncovered approaches are there", not "can we wrap up". An intent is an [open exploration direction] (not a fixed type/menu); judge directions yourself from known facts, assets, and goals, and compare each against open + running + recent_done:
   - already covered by an open/running intent -> do not generate again (being handled).
   - appeared in recent_done -> **first look at that intent's state (each carries one) to tell how it stopped, then decide**:
     - **done (ran to normal completion)**: already covered -> do not re-dispatch as is; whether it is a dead end depends on the fact conclusion it yielded, not on state; re-dispatch only when a [materially new mechanism] appears (a new fact/asset/parameter/clearly different approach), and write the difference from last time clearly in the summary; rewording, or "maybe it will work if I try again", does not count -- no retrying.
     - **exhausted (budget ran out, cut off halfway, only partially written back) / blocked (model or network failure, essentially did not explore)**: both ended badly midway with incomplete information -- first use get_worker_trace / get_worker_output to see what it actually did and where it got stuck, then pick from: nearly broke through but was cut by budget -> dispatch "continue from last progress"; purely external failure that did not run (blocked often is) -> re-dispatch the same direction directly; stuck at the same spot every time -> switch approach/direction. The basis is always the real progress in the trace, not state itself.
   - a brand-new direction covered by no intent at all -> generate.
   - all known directions are covered by intents still open/running -> do not generate, just end (there is running/queued work; wait for it to advance); but if only recent_done covers them, there is no longer any open/running, and the goal is unachieved -> per the hard floor at the top you must open another or continue one.
   - **Depth over coverage**: coverage is a floor/acceptance item, not the exploration goal itself; after finding a high-value entry point (possibly leading to RCE/privilege escalation/data exfiltration), prioritize dispatching intents to [drive that path all the way through] rather than spreading wide to level up coverage with shallow per-asset tests.
   - **Keep routes diverse, do not converge too early**: when the goal is unachieved, if the existing intents all crowd one route/entry, while a [fundamentally different] uncovered direction exists (another entry surface/another asset class/another exploit chain), prioritize adding that divergent direction rather than piling synonymous intents on the same line (look at substantive difference, not wording); if that divergent direction is already covered by an existing intent, still do not generate. The ideal is 2-3 mechanistically different routes coexisting (e.g. "attack via the upload chain" vs. "attack via auth bypass"), concentrating resources only after one produces evidence of [closing in on the goal]. **But diversity always yields to the [operation constraints] at the top**: an entry surface/port/host/action excluded by a constraint must never get an intent, even if it is fundamentally different.

   **Serial exploit chains: dispatch step by step, do not split into parallel.** For a strongly dependent serial chain (step 1 -> step 2 -> step 3, where each step depends on the actual output of the prior one): do not dispatch all at once in parallel (the downstream cannot get a prerequisite that does not exist yet and will only duplicate/spin); use TodoWrite to record the whole chain as todos (one per step), and this round dispatch only the step whose "prerequisites are met" (usually the first), then after it produces a fact, on the next wake-up (the prompt will carry the todo list) dispatch the next step and mark the satisfied one completed. Do not split "the same thing" into two ("confirm the trigger point" and "trigger the trigger point" are the same step); use multiple parallel intents only for [parallel, mutually independent] dimensions (e.g. enumerating multiple unrelated endpoints).

5. **Submit**: use add_intent [once] to batch-submit the new directions you filtered (an intents array, at most the 4 highest-value ones; do not call it repeatedly one by one):
   - **summary**: a one-sentence natural-language description of the direction (full address of the test target + what to do + why); do not force a fixed classification; deduplication relies mainly on comparing it against existing intents.
   - **asset_ids**: the id(s) of the target assets this direction will test/attack (pass them when you can, 0/1/many, from list_assets) -- whenever the direction centers on concrete assets (site/interface/parameter/host), always pass them, for coverage deduplication and linking into the asset graph; pass all of them when spanning multiple assets; leave empty only for purely global recon with no concrete asset.
   - **parent_ids**: which upstream nodes this direction is synthesized from (optional, 0/1/many) -- pass all when multiple facts combine into one intent, pass the id when derived from some upstream intent/finding, and leave empty for a brand-new top-level direction.

Do not repeat, do not force it; but when the goal is unachieved and there is an uncovered, deeper approach, dispatch when you should. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 도메인 도구 + 기본 도구 집합(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash).
	// 자산 커버리지 기능이 꺼져 있으면 add_task_scope/list_untested_assets를 뺀다(프롬프트에 넣지 않는다).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 핵심 태세(방금 끝난 의도 + 미리 가져온 전체 그래프)는 이제 [이번 라운드 user 입력]에 넣는다(아래 input 참고).
	// system 에는 정적 계획 본문만 남긴다. 밖으로 빼면 system이 라운드마다 안정적이라 캐시에 유리하다.
	// 대가는 단일 라운드가 길어지면 태세가 compaction에 눌릴 수 있다는 것(planner 단일 라운드는 보통 짧아 위험이 낮다).
	// situational은 아래 input에 이어 붙는다.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 작업 단위 deadline / 종국 모드(ctx로 주입됨. taskclock.go 참고). 종국 라운드에서는 작업 시간 초과
	// planner 마무리 문구를 [이번 라운드 작업 지시]로 이번 user 입력에 이어 붙여(situational과 함께),
	// 마지막 목표 판정만 하고 새 의도를 내지 않게 한다.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n**Task endgame wrap-up (special instruction this round, overriding the normal planning flow above)**:" + resolveTaskTimeoutWrapup("planner")
	}
	// 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID> 를 먼저 만든다.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 작업 제약 조건(있으면)을 시스템 프롬프트에 주입해 탐색 경계를 정한다
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner는 자체 벽시계 예산이 없다. deadline이 있으면 MaxDuration을 남은 시간으로 좁혀,
	// 작업이 시간에 다다랐을 때 돌고 있는 규획 라운드가 마무리로 들어가게 한다(시간 초과→작업 시간 초과 문구,
	// 단계 한도→per-run 문구).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시를 타 흔적을 남긴다. 프록시 CA를 실어 MITM이 재서명한 HTTPS 인증서를 검증한다
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 웹 검색(선택). ddgs는 키가 필요 없다. brave-free는 BraveKey, tavily는 TavilyKey가 필요하다.
		// WebSearchProxy는 독립 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와 무관하다. 비우면 직접 연결한다.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 하위 명령은 기본으로 프록시+신뢰 CA를 탄다
		WorkingDir:            taskDir,                              // 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=제한 없음; deadline이 있으면=deadline 까지 남은 시간
		Compaction:            compactionConfig(p.compactionWindow()),
		// 깨우기 사이에 공유되는 규획 할 일: 직렬 체인이 여러 라운드에 걸쳐 유지되게 한다(session은 새것, store는 아니다).
		Todos: p.todoFor(ts.ID()),
		// [이번 라운드] 단계 예산에 걸리면 → SDK가 마무리를 돈다: 이번 라운드에 정리된 결론을 반영한다
		// (내야 할 add_intent, 증명 가능한 prove_goal, 직렬 체인은 TodoWrite에 기록). 규획을 멈추는 게 아니다 —
		// planner는 이후에도 반복해서 깨워진다. clamped(작업 deadline에 좁혀짐)일 때는 PromptByReason로 바꾼다(wrapupSettlementForTask 참고).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 이 profile이 비스트리밍을 고르면 Provider.Complete로 간다
		MaxTokens:    p.maxTokens(),    // 0 = 상한을 보내지 않고 서버 기본값에 맡긴다
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 실험 기능: 켜면 noa가 컨텍스트 압축을 맡는다(아카이브는 <workDir>/noa/<SessionID> 아래에 모이며 영속한다).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 태세(방금 끝난 의도 + 전체 그래프)는 이제 이번 라운드 user 입력에 이어 붙는다(아래 input).
	// user 에는 지시 + 깨우기 사이 할 일도 들어간다(할 일은 모델 자신의 규획 메모라 재생 가능하니 user에 둬도 된다).
	// 첫 문장은 [이번 라운드에 구체적 변동이 있었는지]로 둘로 나뉜다: 변동 있음 → 아래 [실제 변동] 블록을 가리킨다.
	// 변동 없음(심장박동 정기 점검 / hint / 복원 등) → "그래프가 바뀌었다"고 거짓말하지 말고, 돌고 있는 의도를 겸사겸사 복검하라고 알린다.
	lead := "A concrete change just occurred (see **The actual change(s) that fired this round** below); plan the next step accordingly:"
	if len(triggers) == 0 {
		lead = "This round is a wake-up from a **scheduled check (heartbeat) / no concrete-change signal** -- the graph may have no new change. While here, review the running intents: use steer_work to course-correct ones with no progress for a long time or going off track, and kill_work to cut losses on ones whose direction is entirely wrong; then judge the goals and decide whether to add directions:"
		// 심장박동/무변동 깨우기에서 전체 그래프에 open 이나 running 의도가 하나도 없으면 → 탐색이 멈춘 것이다
		// (돌고 있는 worker 도, 대기 중인 방향도 없다). planner에 분명히 알리고 이번 라운드에 새 방향을 반드시 내게 해,
		// 돌고 있는 의도만 복검하고 빈 라운드로 끝나지 않게 한다.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a wake-up from a **scheduled check (heartbeat)**, and there is currently **no open or running intent at all** -- no worker is running and no direction is queued, so exploration has stalled. You **must** produce one or more new intents this round that advance the goal and are **mutually distinct** from the existing intents in the graph (you may not produce 0 intents); first judge from the situation below whether the goal is achieved, and if not, add directions immediately:"
		}
	}
	input := lead + situational + "\n\nBased on the situation above, judge the goals. When a goal is [genuinely achieved] (the target result is obtained / the target vulnerability is confirmed), use prove_goal to mark each one. **Hard floor: as long as the goal is unachieved and there is currently no open or running intent (frontier_open=0 and running_intents empty), this round you must produce at least one intent that advances the goal -- here there is no running work to wait on and no queued direction, so producing 0 intents = the task stalls. Only when an open/running intent is already advancing, or the goal is achieved, may you produce no new intent this round.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration은 이제 벽시계가 다하면 돌고 있는 도구를 끊고 그 자리에서(살아 있는 ctx 위) 마무리로 들어간다.
	// 단일 라운드가 멈춰도 마무리를 건너뛰지 않으므로 외부 하드 ctx 보완이 필요 없다. ctx는 pause / kill / shutdown 만 나른다.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
