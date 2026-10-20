package server

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

// countingProvider 는 불린 횟수만 세고 늘 실패하는 가짜 LLM이다. uid를 받지 못한 차례가 LLM까지 가지 않는지 본다.
type countingProvider struct{ calls *atomic.Int32 }

func (p countingProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls.Add(1)
	return llm.Message{}, "", llm.Usage{}, errors.New("countingProvider: LLM이 불리면 안 된다")
}

func (p countingProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.calls.Add(1)
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, errors.New("countingProvider: LLM이 불리면 안 된다"))
	}
}

func newAgentUIDTestTask(t *testing.T, m *Manager, name string) (*Task, int64) {
	t.Helper()
	task, err := m.CreateTask(name, name, nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { _ = m.PG().DeleteTask(taskID) })
	return task, taskID
}

// latestErrorDetail 은 질의가 찾은 마지막 오류 활동의 detail을 돌려준다. 없으면 테스트를 멈춘다.
func latestErrorDetail(t *testing.T, pg *db.DB, query string, arg any) string {
	t.Helper()
	var detail string
	if err := pg.QueryRow(query, arg).Scan(&detail); err != nil {
		t.Fatalf("no error activity explaining the failure: %v", err)
	}
	return detail
}

// uid 분리를 켰는데 풀이 비었으면 작업·대화 차례·메인 에이전트 대화 모두 기다리지 않고 원인이 적힌 오류로 끝난다.
// 풀을 채우는 데 오래 걸려 세 경로를 한 번 채운 풀에서 함께 본다.
func TestAgentUIDPoolExhaustedStopsEveryRunPath(t *testing.T) {
	if testing.Short() {
		t.Skip("풀 전체(1024개)를 채우는 느린 테스트")
	}
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	pg := m.PG()
	ctx := t.Context()

	var used int
	if err := pg.QueryRow(`SELECT count(*) FROM agent_uids`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	for i := used; i < db.AgentUIDPoolSize; i++ {
		c, err := pg.CreateConversation("agent-uid-fill", "agent uid fill", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pg.DeleteConversation(c.ID) })
		if _, err := pg.AcquireConversationAgentUID(ctx, c.ID); err != nil {
			t.Fatalf("fill %d: %v", i+1, err)
		}
	}

	t.Run("engine run fails the task", func(t *testing.T) {
		task, _ := newAgentUIDTestTask(t, m, "agent uid exhausted")
		e := NewEngine(m)
		e.sandbox = &AgentSandbox{}
		t.Cleanup(func() { stopTaskRuntimes(e) })
		e.Run(ctx, task)

		if got := m.TaskStatus(task.ID); got != "failed" {
			t.Fatalf("task status = %q, want failed", got)
		}
		if _, started := e.started.Load(task.ID); started {
			t.Fatal("engine started task routines without an agent uid")
		}
		detail := latestErrorDetail(t, pg, `SELECT COALESCE(detail,'') FROM activity WHERE exploration_id=$1 AND worker='system' AND is_error ORDER BY id DESC LIMIT 1`, task.ExpID)
		if !strings.Contains(detail, db.ErrAgentUIDPoolExhausted.Error()) {
			t.Fatalf("error activity detail = %q, want the pool exhaustion cause", detail)
		}
	})

	t.Run("agent run gets no command environment", func(t *testing.T) {
		_, taskID := newAgentUIDTestTask(t, m, "agent uid exhausted run")
		wireAgentAugment(pg, t.TempDir(), nil, &AgentSandbox{})
		t.Cleanup(func() { agent.ToolAugment = nil })
		_, def, _ := agent.ToolAugment(agent.WithRunInfo(ctx, agent.RunInfo{TaskID: taskID}), "planner")
		if def.CommandEnv != nil || !errors.Is(def.CommandErr, db.ErrAgentUIDPoolExhausted) {
			t.Fatalf("CommandEnv = %v, CommandErr = %v, want no env and the pool exhaustion cause", def.CommandEnv, def.CommandErr)
		}
	})

	t.Run("conversation turn fails without calling the LLM", func(t *testing.T) {
		td := t.TempDir()
		s := newTestServer(t, m, td, td, td)
		s.sandbox = &AgentSandbox{}
		var calls atomic.Int32
		s.cfgMu.Lock()
		s.chatAgent = agent.NewChatAgent(countingProvider{calls: &calls}, "test", s.m.dir, nil, 10000)
		s.cfgMu.Unlock()
		c, err := pg.CreateConversation("agent-uid-exhausted", "agent uid exhausted", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pg.DeleteConversation(c.ID) })

		s.runConversationSync(c, "hello", s.convBusyKey(c.ID))

		if n := calls.Load(); n != 0 {
			t.Fatalf("LLM called %d times without an agent uid", n)
		}
		detail := latestErrorDetail(t, pg, `SELECT COALESCE(detail,'') FROM conversation_activities WHERE conversation_id=$1 AND is_error ORDER BY id DESC LIMIT 1`, c.ID)
		if !strings.Contains(detail, db.ErrAgentUIDPoolExhausted.Error()) {
			t.Fatalf("conversation error detail = %q, want the pool exhaustion cause", detail)
		}
	})

	t.Run("main agent chat fails without calling the LLM or the task", func(t *testing.T) {
		td := t.TempDir()
		s := newTestServer(t, m, td, td, td)
		s.sandbox = &AgentSandbox{}
		var calls atomic.Int32
		s.cfgMu.Lock()
		s.llmProv, s.llmCfg, s.llmOn = countingProvider{calls: &calls}, agent.ConfigFrom("anthropic", "test", "", "sk-test", ""), true
		s.cfgMu.Unlock()
		task, _ := newAgentUIDTestTask(t, m, "agent uid exhausted main agent")
		statusBefore := m.TaskStatus(task.ID)

		r := httptest.NewRequest(http.MethodPost, "/api/chat?task="+task.ID, strings.NewReader(`{"message":"hello"}`))
		w := httptest.NewRecorder()
		s.chat(w, r)
		if w.Code != http.StatusAccepted {
			t.Fatalf("chat status = %d body=%s, want 202 (LLM mode)", w.Code, w.Body.String())
		}
		waitRetestIdle(t, s)

		if n := calls.Load(); n != 0 {
			t.Fatalf("LLM called %d times without an agent uid", n)
		}
		if got := m.TaskStatus(task.ID); got != statusBefore {
			t.Fatalf("task status = %q, want unchanged %q", got, statusBefore)
		}
		detail := latestErrorDetail(t, pg, `SELECT COALESCE(detail,'') FROM activity WHERE exploration_id=$1 AND worker='mainagent' AND is_error ORDER BY id DESC LIMIT 1`, task.ExpID)
		if !strings.Contains(detail, db.ErrAgentUIDPoolExhausted.Error()) {
			t.Fatalf("main agent error detail = %q, want the pool exhaustion cause", detail)
		}
	})
}

// uid를 받지 못한 원인이 풀 고갈이 아니면(여기서는 서버 종료로 ctx 취소) 작업을 failed로 바꾸지 않는다. 그래도
// uid 없이 시작하지 않는다(#177). 원인을 활동에 남기고, 다음 실행에서 다시 받아 시작한다.
func TestEngineRunWaitsForAgentUIDAfterTransientFailure(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	pg := m.PG()
	task, taskID := newAgentUIDTestTask(t, m, "agent uid transient")
	statusBefore := m.TaskStatus(task.ID)

	e := NewEngine(m)
	e.sandbox = &AgentSandbox{}
	t.Cleanup(func() { stopTaskRuntimes(e) })
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	e.Run(canceled, task)

	if got := m.TaskStatus(task.ID); got != statusBefore {
		t.Fatalf("task status = %q, want unchanged %q", got, statusBefore)
	}
	if _, started := e.started.Load(task.ID); started {
		t.Fatal("engine started task routines without an agent uid")
	}
	detail := latestErrorDetail(t, pg, `SELECT COALESCE(detail,'') FROM activity WHERE exploration_id=$1 AND worker='system' AND is_error ORDER BY id DESC LIMIT 1`, task.ExpID)
	if !strings.Contains(detail, context.Canceled.Error()) {
		t.Fatalf("error activity detail = %q, want the transient cause", detail)
	}
	var assigned int
	if err := pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE task_id=$1`, taskID).Scan(&assigned); err != nil {
		t.Fatal(err)
	}
	if assigned != 0 {
		t.Fatalf("agent uid rows for the task = %d, want 0 after the canceled allocation", assigned)
	}

	e.Run(t.Context(), task)
	if _, started := e.started.Load(task.ID); !started {
		t.Fatal("engine did not start the task once the agent uid was assigned")
	}
	if err := pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE task_id=$1`, taskID).Scan(&assigned); err != nil {
		t.Fatal(err)
	}
	if assigned != 1 {
		t.Fatalf("agent uid rows for the task = %d, want 1 after the next run", assigned)
	}
}

// 에이전트 실행마다 ToolAugment가 그 실행의 작업·세션 uid를 명령 환경에 싣는다(#177). 받지 못하면(일시 오류, 속한 곳
// 없음) 도구를 붙이지 않고 CommandErr로 알린다. 에이전트는 그 실행을 시작하지 않는다(agent 쪽 테스트).
func TestToolAugmentCarriesRunAgentUID(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	pg := m.PG()
	_, taskID := newAgentUIDTestTask(t, m, "agent uid run env")
	_, otherTaskID := newAgentUIDTestTask(t, m, "agent uid run env transient")
	c, err := pg.CreateConversation("agent-uid-run-env", "agent uid run env", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteConversation(c.ID) })
	sessionKey := (&Server{}).convBusyKey(c.ID)

	sb := &AgentSandbox{uid: 20000, gid: 20000}
	wireAgentAugment(pg, t.TempDir(), nil, sb)
	t.Cleanup(func() { agent.ToolAugment = nil })
	augment := func(ctx context.Context, ri agent.RunInfo) agent.DeferredInfo {
		t.Helper()
		tools, def, cleanup := agent.ToolAugment(agent.WithRunInfo(ctx, ri), "planner")
		if cleanup != nil {
			cleanup()
		}
		if def.CommandErr != nil && (len(tools) != 0 || def.CommandEnv != nil) {
			t.Fatalf("tools %d, env %v returned with CommandErr %v", len(tools), def.CommandEnv, def.CommandErr)
		}
		return def
	}
	assignedUID := func(column string, id int64) string {
		t.Helper()
		var uid int
		if err := pg.QueryRow(`SELECT uid FROM agent_uids WHERE `+column+`=$1`, id).Scan(&uid); err != nil {
			t.Fatal(err)
		}
		return strconv.Itoa(uid)
	}

	taskDef := augment(t.Context(), agent.RunInfo{TaskID: taskID, IntentID: 7})
	convDef := augment(t.Context(), agent.RunInfo{SessionID: sessionKey})
	for _, tc := range []struct {
		name string
		def  agent.DeferredInfo
		uid  string
	}{
		{name: "task run", def: taskDef, uid: assignedUID("task_id", taskID)},
		{name: "conversation run", def: convDef, uid: assignedUID("conversation_id", c.ID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := []string{AgentUIDEnv + "=" + tc.uid, AgentGIDEnv + "=20000"}
			if tc.def.CommandErr != nil || strings.Join(tc.def.CommandEnv, " ") != strings.Join(want, " ") {
				t.Fatalf("CommandEnv = %v, %v, want %v", tc.def.CommandEnv, tc.def.CommandErr, want)
			}
			if !tc.def.ShareWritesWithGroup {
				t.Fatal("ShareWritesWithGroup = false with the sandbox on")
			}
		})
	}
	if taskDef.CommandEnv[0] == convDef.CommandEnv[0] {
		t.Fatalf("task and conversation share uid %s", taskDef.CommandEnv[0])
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		ri   agent.RunInfo
		want error
	}{
		{name: "transient failure", ctx: canceled, ri: agent.RunInfo{TaskID: otherTaskID}, want: context.Canceled},
		{name: "no task or conversation", ctx: t.Context(), ri: agent.RunInfo{}, want: errRunWithoutOwner},
		{name: "unknown session key", ctx: t.Context(), ri: agent.RunInfo{SessionID: "chat-1"}, want: errRunWithoutOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := augment(tc.ctx, tc.ri)
			if !errors.Is(def.CommandErr, tc.want) || !strings.Contains(def.CommandErr.Error(), agentUIDUnavailableMessage) {
				t.Fatalf("CommandErr = %v, want %v with the user-facing cause", def.CommandErr, tc.want)
			}
		})
	}

	// 꺼져 있으면 지금과 같다. uid를 싣지 않고 막지도 않는다.
	wireAgentAugment(pg, t.TempDir(), nil, nil)
	if def := augment(t.Context(), agent.RunInfo{}); def.CommandEnv != nil || def.CommandErr != nil || def.ShareWritesWithGroup {
		t.Fatalf("sandbox off: %+v", def)
	}
}
