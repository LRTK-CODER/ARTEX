package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 마무리 프롬프트(wrap-up / settlement prompt): agent 가 [단계 소진(MaxTurns)] 또는
// [시간 초과(run_seconds/MaxDuration)]로 종료될 때, SDK 의 settlement 단계가 이 프롬프트를 주입해
// agent 가 식별했지만 아직 써 넣지 않은 것을 먼저 저장하고 요약 한 줄을 내게 한다. 어중간한 끝을 막는다.
//
// 각 agent 의 마무리 프롬프트는 백오피스에서 필요에 따라 덮어쓸 수 있다(agents.wrapup_prompt 에 저장).
// 비우면 여기의 내장 기본값을 쓴다. [프롬프트 본문]만 편집할 수 있다. 어떤 도구를 비활성화할지, 마무리에
// 몇 회합을 줄지는 코드가 고정한 정책이다.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// agent key 로 색인하는 내장 기본 마무리 프롬프트. worker 는 과거에 하드코딩하던
// settleWrapUpPrompt(worker.go 에 정의)를 재사용하고, planner/mainagent 는 각각 한 벌씩 둔다.
// 맞는 것이 없으면(사용자 지정 agent) 범용 기본값으로 간다.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: 각 agent 마무리 단계 [자체]의 회합 예산 내장 기본값(백오피스에서 >0 으로 덮어쓸 수 있다).
// 모두 10회를 줘 마무리 단계에 저장할 단계 수가 넉넉하게 한다. 맞는 것이 없으면 genericWrapupTurns 로 간다.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "The step budget for your planning this round is about to run out -- note that only [this round] ends; the system will still wake you again as the situation changes to keep planning, this is not task termination, and you need not wrap up the whole plan here. Land the conclusions you have already thought through this round and do not let this round go to waste, but also [do not force intents just to wrap up] (0 intents this round is still a perfectly normal result): (1) if you have judged an exploration direction that [should be dispatched now], submit them in one batched add_intent (do not hold back what you have decided); (2) for a goal proven achieved by some finding/fact, call prove_goal to mark it met (do not miss it); (3) if you identified a serial exploit chain that needs to be stepped, record it with TodoWrite so it can be dispatched on the next wake-up. When done, just end this round; no summary text needed."

const mainAgentWrapUpDefault = "Your steps are about to run out and this interaction is ending. Do not start any new exploration/action. In **a single plain-text sentence**, summarize for the user the current progress, the key conclusions, and the suggested next step."

const genericWrapUpDefault = "You are about to be terminated because the budget is exhausted. First write back the results you completed but have not persisted, then in **a single plain-text sentence** summarize what you did and the key conclusions you reached (this sentence is displayed as this run's result)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- 작업 단위 시간 초과 마무리 문구 ----------
//
// per-run 마무리 문구와는 [두 벌]이다: per-run 은 "이번 run 의 예산이 다 찼다"이고, 작업 시간 초과는
// "작업 전체가 시간에 다다라 곧 끝난다"이다. 의미가 종종 반대다(특히 planner: per-run 은 "멈추지 말고 계속
// 규획하라", 작업 시간 초과는 "시간에 다다랐으니 규획을 멈추고 마지막 판정을 하라"). worker/planner 에만 설정한다.

// WrapupTaskTimeoutOverride / …TurnsOverride: 작업 시간 초과 마무리 문구와 회합 수의 DB 덮어쓰기
// (agents.task_timeout_wrapup_prompt / _max_turns 에 연결, worker/planner 만).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (not your run's budget this time -- the whole exploration is up). This is the last chance: (1) persist **all** of what you have identified but not yet written back -- new assets with insert_assets, exploration conclusions/facts with record_fact, confirmed vulnerabilities with report_finding; (2) do not start any new command/probe; (3) **finally, in a single plain-text sentence**, summarize your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (not this round -- the whole task terminates). Based on **all** current facts and findings, make a final goal judgment: for a goal proven achieved by evidence, call prove_goal to mark it met (do not miss it). **Do not generate any new intent** (dispatching one now will not be executed anyway). Once judged, wrap up; no summary text needed."

// TaskTimeoutWrapupDefault 는 어떤 agent 의 작업 시간 초과 내장 기본 마무리 문구를 돌려준다(백오피스 자리표/기본값 복원용).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 설정 안 된 경우(mainagent/chat)는 빈 문자열을 돌려준다
}

// resolveTaskTimeoutWrapup: DB 덮어쓰기(비어 있지 않음) > 내장 기본값. 빈 문자열은 그 agent 에 작업 시간 초과
// 문구가 없다는 뜻이고(worker/planner 가 아님), 이때 호출자는 per-run 문구로 되돌아가야 한다.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 기본은 per-run 회합 수를 따른다
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → 이번 run 이 작업 deadline 에 좁혀짐: Timeout 으로 마무리=작업이 시간에 다다름→작업 시간 초과 문구;
//     MaxTurns 로 마무리=좁혀진 창 안에서 단계가 먼저 소진되고 작업은 몇 분 남음→per-run 문구로 되돌린다.
//   - clamped=false → 작업이 아직 이르다: 두 reason 모두 per-run 문구를 쓴다(즉 wrapupSettlement 로 퇴화).
//
// harness 에 넘긴 PromptByReason 은 마무리 때 [실제] reason 으로 그 자리에서 고른다. build 때 어긋날 일이 없다.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // 기본값(비 clamped 때 두 reason 모두 이 값)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // 작업이 시간에 다다름
				harness.ReasonMaxTurns: perRun, // 단계가 먼저 소진되고 작업은 시간이 남음
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
