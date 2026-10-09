package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// 이 파일은 '내장 도구'를 순수 코드에서, 열거할 수 있고 DB로 덮어쓸 수 있는 목록으로 바꾼다.
//   - BuiltinToolSeeds(): 세 실행 agent의 내장 도구 집합을 seed 레코드(key +
//     설명 + 파라미터 schema + 기본으로 연결된 agent)로 펼쳐, 서버가 부팅 때 tools 표에 멱등으로 시드하게 한다.
//   - ToolResolve 훅: 런타임에 DB의 tools 행에 따라 이미 장착된 도구를 'agent 별 필터링 +
//     설명/schema 덮어쓰기 + 파라미터 기본값 주입' 한다. key/handler는 여전히 코드 층에 있고, DB는 '설명문과 기본값'만 바꾼다.
// handler(Call 동작)는 언제나 코드에서 온다. DB는 그것을 바꾸지 못하고, 모델이 보는 설명과 기본 입력값만 바꿀 수 있다.

// ToolSeed 는 내장 도구 하나의 시드 가능한 스냅숏이다. key는 곧 CoreTool.Name()(handler와 단단히 묶여 있고
// UI 에서는 읽기 전용), Desc/Schema는 코드의 도구 정의에서 가져오며, Agents는 코드가 기본으로 그 도구를 어느 agent에 줬는지다.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), 기본 키, 바꿀 수 없음
	Desc   string         // 최상위 설명(UI 에서 덮어쓸 수 있음)
	Schema map[string]any // 파라미터 JSON-Schema(구조는 읽기 전용, description/default는 UI 에서 바꿀 수 있음)
	Agents []string       // 기본으로 연결된 agent key(worker/planner/mainagent)
}

// builtinToolsByAgent 는 '읽기 전용 빈 껍데기' ToolSet(nil stores)으로 각 실행 agent의
// 도메인 도구 집합을 구성한다. 도구 생성 함수는 클로저를 Spec에 넣기만 하고 생성 시점에 store를 역참조하지 않으므로 nil 이어도 안전하다.
// 이 도구들은 여기서 Name()/Description()/InputSchema()를 읽는 데만 쓰고 절대 Call 하지 않는다.
//
// SDK 공용 도구 actool.DefaultTools()(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)는 일부러 '넣지 않는다'.
// 이들은 모든 agent가 고정으로 가지며 '누구에게 연결할지'를 고를 여지가 없고, 설명 대부분이 Prompt() 안에 있어
// (이 표는 Description() 만 덮으므로 반쪽만 덮는 오해를 낳는다). 시드하지 않음 → DB 행 없음 → ToolResolve가
// 그대로 통과시키고 덮지 않아 기존과 동작이 같다. artex 자체의 도메인 도구만 표에 넣어 관리할 수 있다.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals(목표 분해기)는 기본으로 set_goals + set_constraints를 연결한다. 이들로 분해한 목표와
		// 뽑아낸 작업 제약 조건을 DB에 쓴다. mainagent와 같은 관리 대상 도구를 함께 쓰며, web 에서 설명/schema를 바꾸고 agent 별로 선택할 수 있다.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto는 기본으로 취약점 보고 + 자산 관리 도구를 연결하고, 다른 도메인 도구는 UI 에서 필요에 따라 선택할 수 있다.
		// 새 DB는 이 seed로 쓰이고, 이전 DB는 seedAutoDefaultBindings가 이전한다.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest(독립 침투 agent)는 기본으로 자산 조회 / 자산 추가 / 취약점 보고 / 취약점 조회 / 기업 조회를 연결한다.
		// 새 DB는 이 seed로 쓰이고, 이전 DB는 seedPentestDefaultBindings가 이전한다.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: 이 system 도구들은 평소대로 목록에 들어가지만(web 에서 보이고 agent 별로 직접 선택할 수 있음)
// 기본으로는 '어느 agent 에도 연결하지 않는다'. ToolResolve는 연결이 빈 도구를 모든 agent 에서 일률적으로 버리므로 명시적으로 opt-in 해야 한다.
// 그런데도 어떤 agent의 base 도구 집합에 남겨 두는 이유(예: goal_met이 PlannerTools에 있음)는 둘이다. 하나는 seed가
// 그것을 구성해 desc/schema를 얻게 하려는 것이고, 둘은 사용자가 직접 다시 연결한 뒤 런타임 base에 그것이 있어야 ToolResolve가 유지할 수 있어서다.
//
// goal_met: 하나하나 prove_goal 하는 과정을 건너뛰고 전역에서 바로 '작업 전체 완료'를 선언하므로 영향이 크고 오판 위험이 있으며,
// 'prove_goal로 마지막 목표를 표시 → 자동 마무리'와 겹치기도 한다. 그래서 기본으로는 어느 agent 에도 주지 않고 필요할 때 직접 연결한다.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds 는 각 agent의 내장 도구 집합을 중복 제거해 하나의 seed 목록으로 합친다. 같은 이름의 도구(예: 여러 agent가
// 다 가진 list_assets)는 한 건으로 합치고 Agents는 합집합을 취하며, defaultUnbound의 도구는 연결을 강제로 비운다.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 목록에는 들어가고 직접 연결할 수 있지만 기본으로는 어느 agent 에도 주지 않는다(null이 아니라 []로 저장해 다른 도구와 맞춘다)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// default parameters get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only default values are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
