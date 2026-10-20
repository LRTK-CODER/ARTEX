package server

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

// uid 분리를 켰는데 풀이 비었으면 엔진은 작업 루프를 띄우지 않고, 기다리지 않고 작업을 원인과 함께 failed로 끝낸다.
func TestEngineRunFailsTaskWhenAgentUIDPoolIsExhausted(t *testing.T) {
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

	task, err := m.CreateTask("agent uid exhausted", "agent uid exhausted", nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { _ = pg.DeleteTask(taskID) })

	e := NewEngine(m)
	e.sandbox = &AgentSandbox{}
	e.Run(ctx, task)

	if got := m.TaskStatus(task.ID); got != "failed" {
		t.Fatalf("task status = %q, want failed", got)
	}
	if _, started := e.started.Load(task.ID); started {
		t.Fatal("engine started task routines without an agent uid")
	}
	var detail string
	if err := pg.QueryRow(`SELECT COALESCE(detail,'') FROM activity WHERE exploration_id=$1 AND worker='system' AND is_error ORDER BY id DESC LIMIT 1`,
		task.ExpID).Scan(&detail); err != nil {
		t.Fatalf("no error activity explaining the failure: %v", err)
	}
	if !strings.Contains(detail, db.ErrAgentUIDPoolExhausted.Error()) {
		t.Fatalf("error activity detail = %q, want the pool exhaustion cause", detail)
	}
}
