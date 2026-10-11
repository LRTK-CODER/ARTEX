package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// 이 파일은 '작업 간 오케스트레이션 도구 모음'을 구현한다. 이 도구들은 host
// 도구다. Manager(모든 작업의 Store), Engine(일시 중지), 작업 생성 흐름에 접근해야 하므로 server
// 계층에 둔다. 읽기 도구는 '기존 작업별 도구'를 대상 작업의 store에서 돌게 돌려보낸다(임시 ToolSet을
// 만들어 해당 도구를 Call한다). 그래서 완전히 같은 로직을 재사용한다. 제어 도구(spawn/pause)는
// Manager/Engine을 직접 부른다. 트래픽 도구처럼 tools 테이블에 시드되고 agent별로 연결된다
// (오케스트레이션 agent에만 연결해야 보인다).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // 플랫폼 조작 도구(스킬·도구·MCP 생성과 수정, Auto용)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 불러오기 실패: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id is required"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task not found: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 일반 깨우기(전용 콜백이 없는 쓰기 작업이 쓴다. 읽기 도구에서는 no-op)
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → '사람이 전략 힌트 N건을 더했다: …' 트리거를 기록하고 planner를 깨운다
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"List all tasks (id/description/goal/status/run time/parent task/LLM profile). The orchestration agent uses it to see the big picture, spot tasks stuck too long, and see which LLM each one uses. Run time: running = created→now, terminal = created→last activity (seconds). llm_profile: the profile name the task's planner/worker uses; (active profile) = follows the global active profile.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(active profile)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d (deleted)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"List available LLM profiles: id, name, model, format, and whether it is the current active profile. Pass the id to spawn_task's llm_profile_id parameter to give a subtask its own LLM (e.g. a cheap model for reconnaissance, a strong model for exploitation). Does not include API keys.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"Create a subtask and start the exploration engine; returns task_id. Use it to dispatch one thing (e.g. a challenge or a goal) as an independent task. parent_ref is optional: set it to the parent task id of the current orchestration to link parent and child.",
		objSchema(map[string]any{
			"description":            strParam("Task description (short title)"),
			"goal":                   strParam("Task goal (what to achieve)"),
			"parent_ref":             strParam("Optional: parent task id (links parent and child)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("Optional: list of source task ids to inherit read-only (at most %d). The subtask can reference, read-only, the assets/conclusions those tasks already established as a starting point; unlike parent_ref, which is just a parent-child pointer, this inherits content.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "Optional: LLM profile id for this subtask's planner/worker (see list_llm_profiles); if empty, inherit the parent task's, then fall back to the global active profile"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "Optional: task-level timeout (seconds). When reached, it triggers a graceful wrap-up and the task ends in the timeout state; empty or 0 = unlimited"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "Optional: planner heartbeat interval (seconds). If this long has passed since the last planning round ended / the task started with no trigger in between → trigger a planning round (a fallback against deadlock + a wake-up to supervise in-flight workers). Empty or 0 = default 600 (10 min)."},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "Optional: can be enabled for simple tasks; on creation it dispatches one seed intent (content = description + goal) so a worker starts testing without waiting for the first planner round; default false (the standard plan-then-execute flow)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "이름 없는 작업"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal is required"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 읽기 전용으로 이어받는 출처 작업: 개수 상한과 각 id의 유효성·중복·존재를 HTTP 작업 생성과 같은 규칙으로 검사한다.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("at most %d source tasks can be selected", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("source task id is invalid or duplicated"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("source task #%d not found", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM profile #%d not found or has no API key", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// HTTP로 작업을 만들 때(server.go createTask)와 같은 launchTask를 재사용하는 생성 후 공통 흐름이다.
			// seed + 백그라운드에서 눈에 보이게 하는 목표 분해(0번째 계획/LLM 단계/goal 하나씩) + engine.Run.
			// seed_first_intent는 기본값 false다(표준은 먼저 계획하고 나서 실행). 간단한 작업은 켜서 work 하나를 바로 내려보내 테스트할 수 있다.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "Pause the given task (stop its planner/worker loop).",
		objSchema(map[string]any{"task_id": strParam("id of the task to pause")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task not found: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "Read the given task's exploration-graph overview (same as graph_overview: asset counts/frontier/findings/coverage, etc.); use task_id to specify the task.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "Read the given task's confirmed vulnerabilities (incl. flag/PoC; each carries id/task_id/intent_id/vulnclass/severity/summary/status); use task_id to specify the task.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "Inject a strategic hint into the given task (the task's planner reads it when generating intents on its next round).\n"+
		"PREFER BATCHING: put several hints into the hints array and submit at once (returns an ids array, same length and order as hints, with id=0 for a failed item); for a single hint, omit hints and give the top-level text directly.",
		objSchema(map[string]any{
			"task_id":      strParam("task id"),
			"hints":        map[string]any{"type": "array", "description": "PREFER THIS: array of hints; each element has the same fields as the top level (text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("hint text"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[single] hint text"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "anchored asset ids (optional; 0/1/many; asset ids within this task)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"View the execution trace of one work (intent) in the given task: get_task_worker_trace(task_id, intent_id) shows the step summaries; add step_ids=[...] to fetch the full content of those steps (at most 5 at a time; if more are passed, only the first 5 are returned).",
		objSchema(map[string]any{
			"task_id":   strParam("task id"),
			"intent_id": map[string]any{"type": "integer", "description": "intent id (a work in this task)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "optional: step ids whose full content to fetch (at most 5 at a time; if more are passed, only the first 5 are returned and the rest are listed in omitted_step_ids)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "List which works (intents) have run in the given task plus each one's step count, to find which works are worth reviewing (then use get_task_worker_trace).",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "Search the execution traces of all works in the given task by keyword (returns matched step summaries + intent_id).",
		objSchema(map[string]any{"task_id": strParam("task id"), "q": strParam("search keyword")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"Read the full content of one exploration-graph node in the given task (finding/fact/intent/goal: summary + details/evidence/PoC). id is the exploration node id (e.g. returned by report_finding, or the id in list_task_findings). Use it to fetch a vulnerability's full evidence before writing its report.",
		objSchema(map[string]any{
			"task_id": strParam("task id"),
			"id":      map[string]any{"type": "integer", "description": "exploration-graph node id (not an asset id)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"Write/update the **detailed report** for a recorded vulnerability (full Markdown, overwriting the old content wholesale). Pass the finding_id that report_finding returned (the number in \"finding recorded: <id>\"). The report should cover these sections: `## 개요`, `## 영향`, `## 재현 절차`, `## 증거`/`## PoC`, `## 수정 권고`.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "target vulnerability id (the id report_finding returned)"},
			"report":           strParam("full detailed report, in Markdown"),
			"evidence_version": map[string]any{"type": "integer", "description": "the evidence version returned by get_finding_traffic; prevents the report from overwriting newer evidence changes"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // '숫자 또는 숫자 문자열' 파싱을 재사용한다
			if nodeID <= 0 {
				return actool.Errorf("finding_id is invalid"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("no vulnerability record found for finding_id=%d (record it first with report_finding)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op·platform 도구는 내장 Auto 에이전트에 기본 연결한다(Auto는 본래 플랫폼을 조작하는 역할이다).
	// SeedTool은 첫 삽입에만 적용된다. 구버전 DB에 이미 seed된 행은 seedAutoDefaultBindings가 다시 연결한다.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces를 worker 기본 연결에서 해제한다(일회성)
	s.seedWorkerReadbackRebind()  // 옛 마이그레이션이 잘못 지운 것을 고친다: search_all_worker_traces/get_worker_trace/node_detail를 worker에 다시 연결한다(일회성)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals 프롬프트에 '작업 제약 조건 추출' 단계를 더한다 → 구버전 DB에 새 기본값 버전을 하나 추가한다(일회성)
	s.reseedMainAgentPrompt()         // mainagent 프롬프트에 '목표 달성 뒤 add_intent 때 목표로 등록할지 되묻기'를 더한다(일회성)
	s.reseedPlannerPrompt()           // planner 프롬프트: '0 의도'의 정당한 이유를 다시 쓰고 정량 검수 점검을 더한다(일회성)
	s.reseedWorkerPrompt()            // worker 프롬프트: 부정 결론의 증거 기준을 더한다(일회성)
	reseedPromptsToEnglish(s.m.pg)    // #102: 중국어 기본 프롬프트가 영어로 바뀐 것을 기존 설치의 미수정 기본값 행에 반영(한 번만)
	s.seedReporterAgent()             // '보고서 작성' agent + 도구 연결 + finding 트리거를 미리 준비한다(일회성)
	s.upgradeReporterTriggerMessage() // 구버전 DB 보완 마이그레이션: reporter가 evidence_version을 돌려보내게 한다(일회성)
	s.seedFindingTrafficTools()       // 선택 증거 파라미터와 읽기 전용 증거 도구를 더하고, 사용자 설정은 보존한다
	s.seedFindingWorkflowTools()
	// 참고: pentest의 기본 도구 연결은 마이그레이션이 필요 없다. BuiltinToolSeeds가 완전 새 초기화 때
	// list_assets/insert_assets/report_finding/list_findings/list_companies를 pentest와
	// 함께 seed해 둔다(프로젝트에 아직 구버전 DB가 없어 마이그레이션하지 않는다).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task's llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 아울러 일부 내장 agent 도구를 코드 기본값으로 다시 쓴다:
	//   - goal_met: 구버전 DB에 seed된 설명에 '이번 계획 회차를 끝낸다'는 오해를 부르는 문구가 있어, planner가 이를
	//     '공회전 회차를 끝내는' 수단으로 여기고 막 시작하자마자 작업 전체가 완료됐다고 잘못 판정한다.
	//   - insert_assets: related 파라미터를 새로 더한다(자산이 현재 작업과 관련 있는지 표시하고 커버리지에 넣을지 정한다).
	//     SeedTool은 첫 삽입에만 적용되므로, 구버전 DB에 이미 seed된 schema는 이 새 파라미터를 받지 못한다.
	//   - list_facts: 페이징으로 바꾸며 limit/before/q 파라미터를 더한다. 구버전 DB에 이미 seed된 빈 schema는
	//     도구 관리 페이지에 '파라미터 없음'으로 보이고, 모델도 이 파라미터 설명을 받지 못한다.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform 도구 schema를 코드 기본값으로 다시 썼다(일회성)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met planner 연결 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt 는 goals 목표 분해기의 프롬프트를 '현재 코드 기본값'으로 다시 쓴다. 기본 본문에
// '먼저 작업 제약 조건(set_constraints)을 뽑고 나서 목표를 분해한다'는 단계가 새로 들어갔는데, SeedPromptIfEmpty는
// 첫 삽입에만 적용돼 구버전 DB의 기존 version 1이 이 단계를 받지 못하기 때문이다. 여기서는 버전 관리로 '새 버전을 하나 추가'해
// 그쪽으로 전환한다(ResetPromptToDefault). 옛 버전은 이력에 남아, 사용자가 수정했었다면 버전 기록에서 되찾을 수 있다.
// settings flag로 막아 한 번만 한다. 이후 기본값이 또 바뀌면 이 flag를 올린다. 완전 새 DB는 처리할 필요가 없다(SeedPromptIfEmpty가 이미 최신 기본값을 seed했다).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공하든 실패하든 한 번만 시도한다
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 완전 새 DB라 아직 agent 행이 없으면 seedPrompts가 최신 기본값을 바로 seed하므로 이 마이그레이션이 필요 없다
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 완전 새 DB는 seedPrompts가 이미 최신 기본값을 seed했다 → 현재 버전이 코드 기본값과 같으므로 중복 버전을 또 추가할 필요가 없다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals 프롬프트 새 기본값 다시 쓰기 실패: %v", err)
		return
	}
	log.Printf("[prompts] goals 프롬프트에 새 기본값 버전을 추가했다(작업 제약 조건 추출 단계 추가, 일회성)")
}

// reseedMainAgentPrompt 는 mainagent 프롬프트를 '현재 코드 기본값'으로 다시 쓴다. 기본 본문에 '목표를 모두
// 달성한 뒤 add_intent로 의도를 바로 넣을 때, 정식 목표로 등록할지 사람에게 되묻는다'는 안내가 새로 들어갔는데, SeedPromptIfEmpty는
// 첫 삽입에만 적용돼 구버전 DB의 기존 버전이 받지 못한다. 버전 관리로 '새 버전을 하나 추가'해 그쪽으로 전환한다(ResetPromptToDefault). 옛 버전은
// 이력에 남아, 사용자가 수정했었다면 버전 기록에서 되찾을 수 있다. settings flag로 막아 한 번만 한다. 완전 새 DB는 처리할 필요가 없다
// (SeedPromptIfEmpty가 이미 최신 기본값을 seed했다). reseedGoalsPrompt와 완전히 같은 구조다.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공하든 실패하든 한 번만 시도한다
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 완전 새 DB라 아직 agent 행이 없으면 seedPrompts가 최신 기본값을 바로 seed하므로 이 마이그레이션이 필요 없다
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 완전 새 DB는 seedPrompts가 이미 최신 기본값을 seed했다 → 현재 버전이 코드 기본값과 같으므로 중복 버전을 또 추가할 필요가 없다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent 프롬프트 새 기본값 다시 쓰기 실패: %v", err)
		return
	}
	log.Printf("[prompts] mainagent 프롬프트에 새 기본값 버전을 추가했다(목표 달성 뒤 목표 만들기 되묻기 추가, 일회성)")
}

// reseedPlannerPrompt 는 planner 프롬프트를 '현재 코드 기본값'으로 다시 쓴다. 기본 본문을 간결하게 재구성하고, '절제'를
// 중복 제거만 하는 수준으로 낮췄으며, '깊이가 커버리지보다 우선', '하한선: 목표가 달성되지 않았고 실행 중인 의도가 없으면 반드시 결과물을 낸다'를 더하고, 부정 결론 재검토에 상한을 더했다.
// 기본값이 실질적으로 바뀔 때마다 아래 flag(현재 v2)를 올려 기존 구버전 DB가 다시 한 번 반영하게 한다. SeedPromptIfEmpty는 첫 삽입에만 적용돼 구버전 DB의 기존 버전이 받지 못하므로, 버전 관리로
// '새 버전을 하나 추가'해 그쪽으로 전환한다(ResetPromptToDefault). 옛 버전은 이력에 남아, 사용자가 수정했었다면 버전 기록에서
// 되찾을 수 있다. settings flag로 막아 한 번만 한다. 완전 새 DB는 처리할 필요가 없다(SeedPromptIfEmpty가 이미 최신 기본값을 seed했다).
// reseedGoalsPrompt와 완전히 같은 구조다.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공하든 실패하든 한 번만 시도한다
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 완전 새 DB라 아직 agent 행이 없으면 seedPrompts가 최신 기본값을 바로 seed하므로 이 마이그레이션이 필요 없다
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 완전 새 DB는 seedPrompts가 이미 최신 기본값을 seed했다 → 현재 버전이 코드 기본값과 같으므로 중복 버전을 또 추가할 필요가 없다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner 프롬프트 새 기본값 다시 쓰기 실패: %v", err)
		return
	}
	log.Printf("[prompts] planner 프롬프트에 새 기본값 버전을 추가했다(간결 재구성+절제를 중복 제거로 완화+깊이 우선+부정 재검토 상한, 일회성)")
}

// reseedWorkerPrompt 는 worker 프롬프트를 '현재 코드 기본값'으로 다시 쓴다. 기본 본문의 record_fact 단락에서 '부정 계열 결론은
// 관찰로 적고 탐색적으로 읽는다'는 문장 전체를 지우고, confidence(observed/inferred)를 '이 의도의 수단을 다 썼는지'와 분리했으며(이들은 플래너를 쉽게 오해시킨다),
// 아울러 facts 배열을 여러 항목으로 나누는 것을 '서로 완전히 독립적이어서 합칠 수 없는' 극소수 예외로 좁혔다. flag를 v3으로 올려 기존 구버전 DB가 다시 한 번 반영하게 한다.
// SeedPromptIfEmpty는 첫 삽입에만 적용돼 구버전 DB의 기존 버전이 받지 못하므로, 버전 관리로 '새 버전을 하나 추가'해 그쪽으로 전환한다. 옛 버전은 이력에 남아 되찾을 수 있다.
// settings flag로 막아 한 번만 한다. 완전 새 DB는 처리할 필요가 없다. reseedGoalsPrompt와 완전히 같은 구조다.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공하든 실패하든 한 번만 시도한다
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 완전 새 DB라 아직 agent 행이 없으면 seedPrompts가 최신 기본값을 바로 seed하므로 이 마이그레이션이 필요 없다
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 완전 새 DB는 seedPrompts가 이미 최신 기본값을 seed했다 → 현재 버전이 코드 기본값과 같으므로 중복 버전을 또 추가할 필요가 없다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker 프롬프트 새 기본값 다시 쓰기 실패: %v", err)
		return
	}
	log.Printf("[prompts] worker 프롬프트에 새 기본값 버전을 추가했다(컨텍스트 조회 단락을 list_assets/list_findings로 좁히고 list_facts/node_detail/asset_neighbors 제거, 일회성)")
}

// reporterToolCallMessage 는 보고서를 쓰기 전에 반드시 무조건 get_finding_traffic을 한 번 먼저 읽게 해야 한다.
// 이 도구는 읽기 전용이고 '캡처 스위치에 의존하지 않아', 자동 연결을 켜든 끄든 수동으로 연결한 증거를 읽을 수 있다. 여기를
// '자동 연결을 켜야만 읽는다'로 쓰면, 기본값 꺼짐 설정에서는 reporter가 evidence_version을 보내지 않아
// SetFindingReportVersionByNodeID가 legacy 의미대로 -1을 기록하고, 취약점 상세와 Markdown 내보내기에
// '증거가 변경됨, 보고서 업데이트 필요'가 계속 남으며, UI에는 이를 지울 수 있는 입구가 전혀 없다.
const reporterToolCallMessage = "방금 위에서 취약점 하나가 report_finding으로 등록됐습니다. 반환된 JSON의 finding_id(독립 취약점 기록 ID)와 finding_node_id(탐색 노드 ID)를 읽으세요. " +
	"먼저 get_finding_traffic(finding_id)으로 현재 증거 목록과 그 version을 읽으세요(목록이 비어 있어도 정상이니 평소대로 보고서를 쓰세요). " +
	"운영 지침에서 자동 연결을 켰다면, 읽기 전에 이번 취약점의 트래픽을 먼저 확인해 연결하세요. 노드 상세는 finding_node_id를 쓰세요. " +
	"마지막으로 update_finding_report(finding_id=finding_node_id, report, evidence_version=실제로 읽은 버전)를 호출해 저장하세요. " +
	"evidence_version은 반드시 보내세요. 그러지 않으면 보고서가 영구적으로 '업데이트 필요'로 표시됩니다. 두 가지 번호를 섞어 쓰지 마세요."

// 이전 버전 트리거 메시지(0.3.8 및 그 이전). 지금도 이것과 글자 그대로 같은 기록만 마이그레이션이 덮어쓰고, 사용자가 고친 것은 그대로 둔다.
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage 는 구버전 DB에서 아직 기본 문구인 reporter 트리거 메시지를 새 버전으로 다시 쓴다.
// seedReporterAgent는 reporter_agent_seed_v1으로 막혀 있고 agent를 새로 만들 때만 트리거를 쓰므로,
// 업데이트로 올라온 DB는 새 문구를 받지 못한다. 도구 schema는 seedFindingTrafficTools가
// evidence_version을 채워 넣었지만, reporter에게 그것을 쓰라고 알려 주는 것이 아무것도 없다. 일회성이며, 고치지 않은 문구만 덮어쓴다.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 한 번만 시도한다
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] 트리거 읽기 실패: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // 사용자가 고쳤거나 finding 트리거가 아니면 건드리지 않는다.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 트리거 메시지 업데이트 실패: %v", err)
			return
		}
		log.Printf("[reporter] 트리거 메시지를 evidence_version을 읽어 돌려보내도록 업데이트했다")
	}
}

// seedReporterAgent 는 '보고서 작성' 사용자 지정 agent(builtin=false, UI에서 편집·삭제 가능)를 미리 준비한다.
// update_finding_report + 작업 조회 도구를 연결하고, 'report_finding이 호출되면 바로 실행되는'
// 트리거를 하나 건다. 취약점이 하나 등록될 때마다 이 agent를 깨워 상세 보고서를 쓰게 한다. 일회성이다(settings flag로 막는다). 사용자가 삭제한 뒤에는 다시 만들지 않는다.
// 의존: orchestration 도구는 이 함수 위쪽에서 SeedTool로 이미 저장돼 있어 연결할 수 있다.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공하든 실패하든 한 번만 시도한다

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key가 이미 쓰이고 있으면(사용자가 손수 만든 경우) 덮어쓰지 않는다
	}
	a, err := s.m.pg.CreateAgent("reporter", "보고서 작성",
		"취약점 상세 보고서 작성: 취약점을 발견하면 자동으로 실행되어, 증거와 실행 과정을 조회한 뒤 Markdown 보고서를 작성해 기록한다.")
	if err != nil {
		log.Printf("[reporter] agent 만들기 실패: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt 실패: %v", err)
	}
	// 트리거 실행 정책: parallel + none. 취약점 하나에 보고서 하나, 여러 finding은 병렬로 각각 쓴다.
	// merge는 반드시 none이어야 한다. 아니면(기본값 all) 한 차례의 finding이 한 번의 실행으로 합쳐져 병렬이 의미가 없어진다.
	// maxParallel=5: 동시에 최대 5개 보고서 세션을 두어, 순간적으로 LLM 호출이 너무 많아지는 것을 막는다.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] 트리거 실행 정책 설정 실패: %v", err)
	}
	// 이 agent에 필요한 도구를 연결한다: 보고서 쓰기 + 증거/실행 과정/현황 읽기.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] 도구 연결 실패: %v", err)
	}
	// 트리거: report_finding이 호출되면 바로 실행된다(도구가 "finding recorded: <id>"를 반환하며 finding_id를 함께 싣고,
	// 작업 id도 트리거 메시지에 들어 있다).
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] 트리거 만들기 실패: %v", err)
	}
	log.Printf("[reporter] '보고서 작성' agent + finding 트리거를 미리 준비했다")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker→planner 기본 연결 전환
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope 기본 연결 실패: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope 연결 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s worker 연결 해제 실패: %v", k, err)
			return // 오류가 나면 flag를 세우지 않고 다음 시작 때 다시 시도한다
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: node_detail 추가
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 되돌아보기·상세 도구 다시 연결 실패: %v", err)
		return // 오류가 나면 flag를 세우지 않고 다음 시작 때 다시 시도한다
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 옛 자산 도구 이름 교체, insert_assets/add_company_scope 추가
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// 자산 도구: Auto는 플랫폼을 조작하며 자산을 자주 보고/등록하고 기업 범위를 관리한다.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 기본 연결 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
