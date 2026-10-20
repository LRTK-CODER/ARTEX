package server

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// flakyTaskUID 는 처음 failures번은 DB 일시 오류처럼 실패하고 그 뒤로는 실제 풀에서 uid를 받는 가짜다. 실제처럼 끝난
// ctx에는 ctx 오류를 올린다.
type flakyTaskUID struct {
	pg       *db.DB
	failures atomic.Int32
	calls    atomic.Int32
}

var errAgentUIDTestTransient = errors.New("connection reset by peer")

func (f *flakyTaskUID) acquire(ctx context.Context, taskID int64) (int, error) {
	f.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("assign agent uid to task %d: %w", taskID, err)
	}
	if f.failures.Add(-1) >= 0 {
		return 0, fmt.Errorf("assign agent uid to task %d: %w", taskID, errAgentUIDTestTransient)
	}
	return (&AgentSandbox{}).TaskUID(ctx, f.pg, taskID)
}

// fakeUIDClock 은 재시도 대기를 테스트가 직접 넘기는 가짜 시계다. 엔진이 기다리기 시작하면 그 간격을 waits로
// 알리고, 테스트가 tick을 보내야 대기가 끝난다.
type fakeUIDClock struct {
	now   time.Time
	waits chan time.Duration
	tick  chan time.Time
}

func newFakeUIDClock() *fakeUIDClock {
	return &fakeUIDClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local),
		waits: make(chan time.Duration, 8), tick: make(chan time.Time)}
}

func (c *fakeUIDClock) backoff() agentUIDBackoff {
	return agentUIDBackoff{now: func() time.Time { return c.now }, after: func(d time.Duration) <-chan time.Time {
		c.waits <- d
		return c.tick
	}}
}

func (c *fakeUIDClock) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.waits:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not wait for the next agent uid attempt")
		return 0
	}
}

// waitForCondition 은 다른 goroutine이 바꾸는 엔진 상태를 기다린다. 5초 안에 참이 되지 않으면 테스트를 멈춘다.
func waitForCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// agentUIDActivities 는 작업 활동 가운데 uid를 받지 못했다는 system 오류 활동의 요약을 돌려준다.
func agentUIDActivities(t *testing.T, pg *db.DB, expID int64) []string {
	t.Helper()
	rows, err := pg.Query(`SELECT summary FROM activity WHERE exploration_id=$1 AND worker='system' AND is_error AND summary LIKE $2 ORDER BY id`,
		expID, agentUIDUnavailableMessage+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var summaries []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		summaries = append(summaries, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return summaries
}

// uid를 일시 오류로 받지 못한 작업은 사용자가 다시 열지 않아도 그 작업의 실행 자리에서 물러나며 다시 받고, 받으면
// 시작한다. 그동안 작업 상태는 그대로이고, 원인과 다음 시도 시각은 활동에 한 번만 남는다. 일시 중지하면 다음 시도를
// 기다리지 않고 끝난다.
func TestEngineRunRetriesAgentUIDAfterTransientFailure(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	pg := m.PG()

	newEngine := func(t *testing.T, failures int32) (*Engine, *flakyTaskUID, *fakeUIDClock) {
		e := NewEngine(m)
		e.sandbox = &AgentSandbox{}
		flaky := &flakyTaskUID{pg: pg}
		flaky.failures.Store(failures)
		e.taskUID = flaky.acquire
		clock := newFakeUIDClock()
		e.uidBackoff = clock.backoff()
		t.Cleanup(func() { stopTaskRuntimes(e) })
		return e, flaky, clock
	}

	t.Run("starts once the uid arrives after two transient failures", func(t *testing.T) {
		task, taskID := newAgentUIDTestTask(t, m, "agent uid retry")
		statusBefore := m.TaskStatus(task.ID)
		e, flaky, clock := newEngine(t, 2)

		e.Run(t.Context(), task)
		if e.Started(task.ID) {
			t.Fatal("engine started task routines without an agent uid")
		}
		if d := clock.nextWait(t); d != agentUIDRetryInitial {
			t.Fatalf("first retry wait = %s, want %s", d, agentUIDRetryInitial)
		}
		e.Run(t.Context(), task) // 다시 열어도 재시도를 겹쳐 띄우거나 활동을 더 남기지 않는다
		clock.tick <- clock.now
		if d := clock.nextWait(t); d != 2*agentUIDRetryInitial {
			t.Fatalf("second retry wait = %s, want %s", d, 2*agentUIDRetryInitial)
		}
		if got := m.TaskStatus(task.ID); got != statusBefore {
			t.Fatalf("task status while retrying = %q, want unchanged %q", got, statusBefore)
		}
		clock.tick <- clock.now

		waitForCondition(t, "the task to start", func() bool { return e.Started(task.ID) })
		if n := flaky.calls.Load(); n != 3 {
			t.Fatalf("uid attempts = %d, want 3 (two transient failures, then success)", n)
		}
		var assigned int
		if err := pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE task_id=$1`, taskID).Scan(&assigned); err != nil {
			t.Fatal(err)
		}
		if assigned != 1 {
			t.Fatalf("agent uid rows for the task = %d, want 1", assigned)
		}
		summaries := agentUIDActivities(t, pg, task.ExpID)
		if len(summaries) != 1 {
			t.Fatalf("agent uid activities = %q, want exactly one", summaries)
		}
		if next := clock.now.Add(agentUIDRetryInitial).Format("15:04:05"); !strings.Contains(summaries[0], next) {
			t.Fatalf("activity %q does not name the next attempt time %s", summaries[0], next)
		}
	})

	t.Run("pause ends the retry without waiting for the next attempt", func(t *testing.T) {
		task, taskID := newAgentUIDTestTask(t, m, "agent uid retry pause")
		e, flaky, clock := newEngine(t, 1)

		e.Run(t.Context(), task)
		clock.nextWait(t)
		e.Pause(task.ID, agent.AbortPausedByUser)

		waitForCondition(t, "the uid retry to end", func() bool {
			_, retrying := e.uidRetries.Load(task.ID)
			return !retrying
		})
		if e.Started(task.ID) {
			t.Fatal("paused task started")
		}
		if n := flaky.calls.Load(); n != 1 {
			t.Fatalf("uid attempts = %d, want 1 (no attempt after the pause)", n)
		}
		var assigned int
		if err := pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE task_id=$1`, taskID).Scan(&assigned); err != nil {
			t.Fatal(err)
		}
		if assigned != 0 {
			t.Fatalf("agent uid rows for the task = %d, want 0", assigned)
		}
	})

	// 일시 중지가 재시도 표시를 바로 지우므로, 옛 재시도 루틴이 끝나기 전에 Run이 불려도 물러나지 않고 다시 받는다.
	// 예전에는 남은 표시를 보고 물러나 작업이 루프도 재시도도 없이 남았다.
	t.Run("run right after pause acquires again instead of yielding to the ending retry", func(t *testing.T) {
		task, _ := newAgentUIDTestTask(t, m, "agent uid retry pause run")
		e, flaky, clock := newEngine(t, 1)

		e.Run(t.Context(), task)
		clock.nextWait(t)
		e.Pause(task.ID, agent.AbortPausedByUser)
		e.Run(t.Context(), task)
		e.Resume(task)

		waitForCondition(t, "the task to start", func() bool { return e.Started(task.ID) })
		if n := flaky.calls.Load(); n != 2 {
			t.Fatalf("uid attempts = %d, want 2 (one transient failure, then Run after the pause)", n)
		}
		if _, retrying := e.uidRetries.Load(task.ID); retrying {
			t.Fatal("uid retry still registered after the task started")
		}
	})

	// 이미 루프를 띄운 작업을 다시 열 때는 uid를 다시 확인하지 않는다. 그때 DB가 일시 오류여도 재시도 루틴이나
	// "받으면 작업을 시작합니다" 활동이 생기지 않는다.
	t.Run("run on a started task skips the uid check", func(t *testing.T) {
		task, _ := newAgentUIDTestTask(t, m, "agent uid started rerun")
		e, flaky, _ := newEngine(t, 0)

		e.Run(t.Context(), task)
		if !e.Started(task.ID) {
			t.Fatal("task did not start with an agent uid")
		}
		flaky.failures.Store(1)
		e.Run(t.Context(), task)

		if n := flaky.calls.Load(); n != 1 {
			t.Fatalf("uid attempts = %d, want 1 (no check on the second Run)", n)
		}
		if _, retrying := e.uidRetries.Load(task.ID); retrying {
			t.Fatal("second Run on a started task registered a uid retry")
		}
		if summaries := agentUIDActivities(t, pg, task.ExpID); len(summaries) != 0 {
			t.Fatalf("agent uid activities = %q, want none", summaries)
		}
	})
}

// 대화 차례는 uid를 풀 고갈이 아닌 오류로 받지 못해도 계속하지 않고 LLM을 부르지 않는다(#177). 일시 오류를
// 결정적으로 만들 방법이 없어, 같은 분기로 가는 다른 DB 오류(차례 도중 대화가 지워져 uid 행의 외래 키가 깨짐)를 쓴다.
// 이미 취소된 ctx는 uid를 받기 전에 차례가 끝나 이 분기에 닿지 않는다.
func TestConversationTurnStopsWhenAgentUIDFailsWithOtherError(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	pg := m.PG()
	td := t.TempDir()
	s := newTestServer(t, m, td, td, td)
	s.sandbox = &AgentSandbox{}
	var calls atomic.Int32
	s.cfgMu.Lock()
	s.chatAgent = agent.NewChatAgent(countingProvider{calls: &calls}, "test", s.m.dir, nil, 10000)
	s.cfgMu.Unlock()
	c, err := pg.CreateConversation("agent-uid-other-error", "agent uid other error", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.DeleteConversation(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sandbox.ConversationUID(t.Context(), pg, c.ID); err == nil || errors.Is(err, db.ErrAgentUIDPoolExhausted) {
		t.Fatalf("ConversationUID for a deleted conversation = %v, want a non-exhaustion error", err)
	}

	s.runConversationSync(c, "hello", s.convBusyKey(c.ID))

	if n := calls.Load(); n != 0 {
		t.Fatalf("LLM called %d times without an agent uid", n)
	}
	var assigned int
	if err := pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE conversation_id=$1`, c.ID).Scan(&assigned); err != nil {
		t.Fatal(err)
	}
	if assigned != 0 {
		t.Fatalf("agent uid rows for the conversation = %d, want 0", assigned)
	}
}
