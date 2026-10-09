package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// promptVersionCount 는 한 agent 의 agent_prompts 버전 개수를 센다. 마이그레이션이 새
// 버전을 추가했는지(기본값 행), 또는 건드리지 않았는지(사용자 수정 행)를 확인하는 데 쓴다.
func promptVersionCount(t *testing.T, pg *db.DB, agentID int64) int {
	t.Helper()
	var n int
	if err := pg.QueryRow(`SELECT count(*) FROM agent_prompts WHERE agent_id=$1`, agentID).Scan(&n); err != nil {
		t.Fatalf("버전 개수 조회 = %v, 기대값 nil", err)
	}
	return n
}

// seedTestAgent 는 고유 키의 테스트용 agent 를 만들고 active 프롬프트를 body 로 씨앗으로 넣는다.
// 끝나면 그 agent 와 프롬프트 행을 지운다.
func seedTestAgent(t *testing.T, pg *db.DB, key, body string) *db.Agent {
	t.Helper()
	a, err := pg.CreateAgent(key, key, "")
	if err != nil {
		t.Fatalf("CreateAgent(%q) = %v, 기대값 nil", key, err)
	}
	if _, err := pg.SavePrompt(a.ID, body, "test seed", "system"); err != nil {
		t.Fatalf("SavePrompt = %v, 기대값 nil", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1`, a.ID)
		_, _ = pg.Exec(`DELETE FROM agents WHERE id=$1`, a.ID)
	})
	return a
}

// TestMigratePromptsToEnglish 는 번역 전 중국어 기본값 그대로인 행만 영어 기본값으로
// 바뀌고, 사용자가 고친 행은 그대로이며, 두 번 돌려도 같음을 확인한다(#102).
func TestMigratePromptsToEnglish(t *testing.T) {
	skipWithoutPostgres(t)
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres not configured: %v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	uniq := time.Now().UnixNano()
	seeds := agent.BuiltinPromptSeeds()

	// (1) 번역 전 중국어 기본값 그대로인 worker 기본값 행.
	defaultAgent := seedTestAgent(t, pg, fmt.Sprintf("t102def_%d", uniq), legacyWorkerZH)
	// (2) 사용자가 고친 행(중국어 기본값과 다름).
	const editedBody = "my own custom worker prompt, do not touch"
	editedAgent := seedTestAgent(t, pg, fmt.Sprintf("t102edit_%d", uniq), editedBody)
	// (3) 사용자 지정 대화 agent 의 기본값(assistant) 그대로인 행.
	assistantAgent := seedTestAgent(t, pg, fmt.Sprintf("t102asst_%d", uniq), legacyAssistantZH)

	defVersions := promptVersionCount(t, pg, defaultAgent.ID)
	editVersions := promptVersionCount(t, pg, editedAgent.ID)

	migratePromptsToEnglish(pg)

	// 기본값 행 → 영어 기본값으로 바뀐다(새 버전 추가).
	if got, _ := pg.CurrentPrompt(defaultAgent.ID); got != seeds["worker"] {
		t.Fatalf("기본값 worker 행 active = %q, 기대값 영어 기본값 %q", firstN(got, 60), firstN(seeds["worker"], 60))
	}
	if got := promptVersionCount(t, pg, defaultAgent.ID); got != defVersions+1 {
		t.Fatalf("기본값 worker 행 버전 수 = %d, 기대값 %d(새 버전 1개 추가)", got, defVersions+1)
	}
	// assistant 기본값 행 → 영어 기본값으로 바뀐다.
	if got, _ := pg.CurrentPrompt(assistantAgent.ID); got != agent.DefaultAssistantPrompt {
		t.Fatalf("기본값 assistant 행 active = %q, 기대값 영어 기본값", firstN(got, 60))
	}
	// 사용자 수정 행 → 그대로.
	if got, _ := pg.CurrentPrompt(editedAgent.ID); got != editedBody {
		t.Fatalf("사용자 수정 행 active = %q, 기대값 그대로 %q", firstN(got, 60), editedBody)
	}
	if got := promptVersionCount(t, pg, editedAgent.ID); got != editVersions {
		t.Fatalf("사용자 수정 행 버전 수 = %d, 기대값 %d(변화 없음)", got, editVersions)
	}

	// 멱등: 한 번 더 돌려도 바뀐 행은 영어 그대로이고 새 버전이 더 생기지 않는다.
	afterDefVersions := promptVersionCount(t, pg, defaultAgent.ID)
	migratePromptsToEnglish(pg)
	if got, _ := pg.CurrentPrompt(defaultAgent.ID); got != seeds["worker"] {
		t.Fatalf("재실행 후 worker 행 active = %q, 기대값 영어 기본값 유지", firstN(got, 60))
	}
	if got := promptVersionCount(t, pg, defaultAgent.ID); got != afterDefVersions {
		t.Fatalf("재실행 후 worker 행 버전 수 = %d, 기대값 %d(새 버전 없음=멱등)", got, afterDefVersions)
	}
	if got, _ := pg.CurrentPrompt(editedAgent.ID); got != editedBody {
		t.Fatalf("재실행 후 사용자 수정 행 active = %q, 기대값 그대로", firstN(got, 60))
	}
}

func firstN(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
