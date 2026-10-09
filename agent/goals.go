package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl 은 목표 분해기 프롬프트의 내장 편집 가능 본문(섹션 [A])이며
// agent_prompts에 씨앗으로 들어간다. 지금은 템플릿 변수를 쓰지 않는다.
const goalsDefaultTmpl = `You are a penetration-testing goal decomposer. Your job is to identify from the user input the **final results to be achieved**, not to plan attack steps.

**Step 1 (do this before splitting goals): extract operation constraints**
From the "task goal / task description", identify the operator's explicit rules about **what actions are and are not allowed**, and register each with set_constraints (if the description and goal involve no operation constraints, you may skip extracting them):
- type=deny: forbidden actions (e.g. "do not scan ports", "no write/delete operations against production", "no brute forcing", "do not touch a certain subdomain").
- type=allow: explicitly allowed/scoped actions (e.g. "passive reconnaissance only", "only against a certain domain").
- A constraint is neither a goal nor an attack step: it is a rule about the boundary of operational behavior.
- **A constraint must be [self-contained, with the concrete target written in]**: replace referential words like "the current goal/current port/current IP/current domain/this site" with the **concrete values** from the task goal/description. Constraints are injected separately into the execution-phase prompt, where a referential word cannot tell what it refers to once out of context.
  Example: the goal is https://abc.example.net -> write "only testing abc.example.net is allowed" rather than "only testing the current target is allowed"; "only test the target port 443, do not scan other ports" rather than "only test the current port". If the original says only "the current target" but the target address is already clear, fill the address in.
- **Register only constraints [explicitly written or emphasized] in the goal/description; never invent them**; when unsure of the type, use deny (more conservative).
- If the goal/description truly has no operation constraints, **do not** call set_constraints.
After registering constraints (if any), proceed to the goal decomposition below.

**Goal = a final deliverable/verifiable result**

**What is NOT a goal (do not list as a sub-goal)**:
- information gathering, reconnaissance, endpoint scanning
- vulnerability analysis and verification process
- attack steps, exploitation techniques
- result verification steps

**Decomposition principles**:
- the user describes only one final goal -> output one
- there are multiple **mutually independent** final deliverables -> list them separately
- tag vulnclass when a clear vulnerability class applies; leave it empty for information-gathering/business-logic goals
- never invent goals the user did not mention

Call set_goals to submit the result.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: register the test asset scope**
Besides decomposing goals, you must identify the **explicitly given test asset scope** from the "task goal / task description" and register it with add_task_scope (this is the task's authorized boundary, and also the denominator of asset test coverage). **Minimal-scope principle: register only the single target the user explicitly pointed to, and never widen it on your own.**
- the goal is a URL or an address with a hostname (e.g. https://xxx.example.com/path, app.example.com) -> take its **full hostname**, kind=subdomain, value=full hostname.
  Example: goal https://a1b2c3.lab.example.net/path -> kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Never** shorten a hostname with a subdomain to the root domain -- registering the whole example.com when you see xxx.example.com widens the scope beyond the user's target and violates the minimal-scope principle.
- only when what the user gives is a **bare root domain with no subdomain** (e.g. example.com written directly), or they explicitly say "the whole site / all subdomains / the entire domain" -> use kind=root_domain, value=example.com.
- a plain IP or network range -> kind=ip / cidr, value=IP or CIDR.
- **Do not** register a company scope (company) -- the task was just created and the asset system usually does not have this company yet, so it cannot be registered; leave company-level scope to the later plan phase.
Other rules:
- Register only the scope **explicitly written** in the goal/description; never invent or infer domains/IPs not mentioned.
- In reason, briefly state which sentence it is based on, for auditing.
- If the goal/description has no explicit asset scope, **do not** call add_task_scope.
First register the scope with add_task_scope (if any), then call set_goals to submit the goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals 는 LLM 에게 침투 테스트 작업 목표를 서로 독립적으로 검증 가능한
// 개별 목표(각각 하나의 goal 노드가 된다)로 쪼개 달라고 한다. 제공자가 설정되지 않았거나
// 호출이 아무것도 내놓지 못하면 nil을 돌려준다 — 그러면 호출자가 규칙 기반 분해로
// 되돌아가 goal 노드가 늘 존재하게 한다.
//
// prov는 호출자가 건넨다(여기서 Config로 만들지 않는다). 그래서 목표 분해가 엔진의
// 나머지와 같은 제공자 인스턴스를 타고 돈다 — 같은 속도 제한기를 공유하고, llmrec에
// 기록되며, LLM 실패 전환에 참여한다. 이 셋을 조용히 건너뛰지 않는다.
//
// desc는 작업의 자유 서술 설명(배경: 대상 범위/flag 개수/교전 설명 등)이다. 목표와 함께
// 넣어 분해기가 더는 맹목적으로 쪼개지 않게 한다 — 프롬프트는 두 텍스트에 없는 것을
// 지어내는 것을 여전히 금지한다.
//
// emit은 nil이 아니면 Worker="planner" 로 모든 LLM 단계(thinking/tool_use/result)를 받아,
// 0번째 라운드의 목표 분해 활동이 UI에 보이게 한다.
//
// as + taskID는 nil이 아니거나 양수이면 add_task_scope 도구를 연결해, 분해기가 목표에서
// 뽑아낸 명시적 자산 범위를 등록할 수 있게 한다.
//
// ts는 작업의 탐색 저장소다: set_goals가 분해한 goal 노드를 바로 여기에 쓴다(메인 agent가
// 런타임에 목표를 추가할 때 쓰는 것과 같은 관리 도구다). 돌려주는 spec은 저장소에서 다시
// 읽어 오므로, 호출자가 목표별 활동을 내보내고 "LLM이 아무것도 못 냈다"는 경우를 감지해
// 되돌아갈 수 있다.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 목표 분해는 일회성 호출이다: transcript store를 달지 않으므로 agentcore가 ctx에
	// session id를 달지 않는다(writer가 있을 때만 단다. agentcore.Prompt 참고). 그런데
	// session-id 헤더로 프롬프트 캐시/고정 라우팅을 하는 게이트웨이(opencode zen은
	// x-opencode-session이 없으면 바로 400 MissingSessionID)는 ctx의 이 값을 읽는다 —
	// 채우지 않으면 "대화는 정상, 분해만 400" 이 된다. 안정적인 id를 명시적으로 단다: 같은
	// 탐색의 분해 요청이 그것을 공유해(캐시 적중에 유리) planner/worker와 충돌하지 않고,
	// llmrec.parseSession이 올바로 귀속할 수 있다.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints는 늘 쓸 수 있다(asset store에 의존하지 않는다). 본문에 이미 "먼저
	// 작업 제약 조건을 뽑고 목표를 쪼개라"는 단계가 있으니(agent 편집 페이지에서 표현을 바꿀
	// 수 있다) 여기서는 도구만 연결하면 된다.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background; may include target scope / number of flags / engagement notes; for reference only, do not invent anything not mentioned in it):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3단계(제약 조건 추출 → 범위 등록 → 목표 분해)마다 도구 호출이 한 번씩 필요하다.
		// 회합을 넉넉히 줘 마무리 전에 set_goals 호출을 빠뜨리지 않게 한다.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // 이 profile이 비스트리밍을 고르면 Provider.Complete로 간다
		MaxTokens:    maxTokens,    // 0 = 상한을 보내지 않고 서버 기본값에 맡긴다
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
