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

// uid를 받지 못한 원인이 풀 고갈이 아니면(여기서는 서버 종료로 ctx 취소) 작업을 failed로 바꾸지 않고 #175처럼
// 시작한다. uid는 다음 실행에서 다시 받는다.
func TestEngineRunKeepsTaskWhenAgentUIDFailsTransiently(t *testing.T) {
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
	if _, started := e.started.Load(task.ID); !started {
		t.Fatal("engine did not start the task after a transient agent uid failure")
	}
	var assigned int
	if err := pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE task_id=$1`, taskID).Scan(&assigned); err != nil {
		t.Fatal(err)
	}
	if assigned != 0 {
		t.Fatalf("agent uid rows for the task = %d, want 0 after the canceled allocation", assigned)
	}

	e.Run(t.Context(), task)
	if err := pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE task_id=$1`, taskID).Scan(&assigned); err != nil {
		t.Fatal(err)
	}
	if assigned != 1 {
		t.Fatalf("agent uid rows for the task = %d, want 1 after the next run", assigned)
	}
}
