package server

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

// openReseedTestDB 는 실제 PostgreSQL에 연결한다. 설정이 없거나 연결하지 못하면 건너뛴다.
func openReseedTestDB(t *testing.T) *db.DB {
	t.Helper()
	skipWithoutPostgres(t)
	dsn, _, err := db.DSN(config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres not configured: %v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	return pg
}

// TestRefreshLegacyChineseDefaults 는 번역 전 설치에 남은 reporter 트리거의 옛 중국어 기본 문구(V1, V2),
// 프롬프트 버전 메모, 작업-자산 연결 출처 설명이 글자가 정확히 같을 때만 한국어로 바뀌고,
// 사용자가 고친 값과 프롬프트 본문은 그대로이며, 두 번 돌려도 같음을 확인한다(#170).
func TestRefreshLegacyChineseDefaults(t *testing.T) {
	pg := openReseedTestDB(t)
	uniq := time.Now().UnixNano()

	agentKey := fmt.Sprintf("t170_reporter_%d", uniq)
	var agentID int64
	if err := pg.QueryRow(`INSERT INTO agents(key, name, description, role, builtin, enabled) VALUES ($1,'t170','t170','assistant',false,false) RETURNING id`,
		agentKey).Scan(&agentID); err != nil {
		t.Fatalf("에이전트 INSERT = %v, 기대값 nil", err)
	}
	t.Cleanup(func() {
		_ = pg.DeleteTriggersForAgent(agentKey)
		_, _ = pg.Exec(`DELETE FROM agents WHERE id=$1`, agentID)
	})

	// 트리거 메시지: 옛 기본 문구는 한국어 기본값으로, 나머지는 그대로다.
	const userEditedMessage = "上面刚有一个漏洞被 report_finding 登记。我自己改过这段话。"
	type triggerCase struct {
		name        string
		isToolCall  bool
		message     string
		wantMessage string
	}
	triggerCases := []triggerCase{
		{"v2_chinese_default", true, reporterToolCallMessageV2, reporterToolCallMessage},
		{"v1_chinese_default", true, reporterToolCallMessageV1, reporterToolCallMessage},
		{"user_edited_chinese", true, userEditedMessage, userEditedMessage},
		{"korean_default", true, reporterToolCallMessage, reporterToolCallMessage},
		{"not_tool_call", false, reporterToolCallMessageV2, reporterToolCallMessageV2}, // 도구 호출 트리거가 아니면 대상이 아니다
	}
	triggerIDs := map[string]int64{}
	for _, tc := range triggerCases {
		created, err := pg.CreateTrigger(&db.AgentTrigger{AgentKey: agentKey, Enabled: true, OnToolCall: tc.isToolCall,
			ToolNames: []string{"report_finding"}, ToolCallMessage: tc.message})
		if err != nil {
			t.Fatalf("트리거 %s 만들기 = %v, 기대값 nil", tc.name, err)
		}
		triggerIDs[tc.name] = created.ID
	}

	// 프롬프트 버전 메모: 글자가 같은 옛 메모만 바뀌고 본문은 그대로다.
	const userNote = "内置默认，我改过"
	type promptCase struct {
		name     string
		note     sql.NullString
		wantNote sql.NullString
	}
	note := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	promptCases := []promptCase{
		{"builtin_default", note("内置默认"), note(db.PromptNoteBuiltinDefault)},
		{"reset_default", note("恢复为内置默认"), note(db.PromptNoteResetDefault)},
		{"user_note", note(userNote), note(userNote)},
		{"null_note", sql.NullString{}, sql.NullString{}},
	}
	const chineseTemplate = "内置默认" // 본문이 옛 메모와 같아도 본문은 바꾸지 않는다
	promptIDs := map[string]int64{}
	for i, pc := range promptCases {
		var id int64
		if err := pg.QueryRow(`INSERT INTO agent_prompts(agent_id, version, template_text, note, updated_by) VALUES ($1,$2,$3,$4,'system') RETURNING id`,
			agentID, i+1, chineseTemplate, pc.note).Scan(&id); err != nil {
			t.Fatalf("프롬프트 %s INSERT = %v, 기대값 nil", pc.name, err)
		}
		promptIDs[pc.name] = id
	}

	// 작업-자산 연결 출처 설명: 글자가 같은 옛 문구만 바뀐다.
	task, err := pg.CreateTask("t170 legacy summaries", "goal", nil, 0, 0)
	if err != nil {
		t.Fatalf("CreateTask = %v, 기대값 nil", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
	type linkCase struct {
		name        string
		summary     string
		wantSummary string
	}
	const userSummary = "任务执行期间自动关联（我改过）"
	linkCases := []linkCase{
		{"auto", "任务执行期间自动关联", db.TaskAssetLinkSummaryAuto},
		{"legacy", "由历史任务资产关联迁移", db.TaskAssetLinkSummaryLegacy},
		{"user_summary", userSummary, userSummary},
	}
	assets := pg.Assets()
	assetIDs := map[string]int64{}
	for _, lc := range linkCases {
		id, err := assets.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("t170-%s-%d.example.test", lc.name, uniq)})
		if err != nil {
			t.Fatalf("자산 %s 만들기 = %v, 기대값 nil", lc.name, err)
		}
		t.Cleanup(func() { _, _ = assets.DeleteByIDs([]int64{id}) })
		if _, err := assets.AttachAssetsToTask(task.ID, []int64{id}, "t170"); err != nil {
			t.Fatalf("자산 %s 연결 = %v, 기대값 nil", lc.name, err)
		}
		if _, err := pg.Exec(`UPDATE task_asset_links SET source_summary=$3 WHERE task_id=$1 AND asset_id=$2`, task.ID, id, lc.summary); err != nil {
			t.Fatalf("자산 %s 출처 설명 설정 = %v, 기대값 nil", lc.name, err)
		}
		assetIDs[lc.name] = id
	}

	for run := 1; run <= 2; run++ { // 두 번째 실행은 멱등 확인이다
		if ok := refreshLegacyChineseDefaults(pg, agentKey); !ok {
			t.Fatalf("%d번째 refreshLegacyChineseDefaults = false, 기대값 true", run)
		}
		for _, tc := range triggerCases {
			t.Run(fmt.Sprintf("run%d/trigger/%s", run, tc.name), func(t *testing.T) {
				var message string
				if err := pg.QueryRow(`SELECT tool_call_message FROM agent_triggers WHERE id=$1`, triggerIDs[tc.name]).Scan(&message); err != nil {
					t.Fatalf("조회 = %v, 기대값 nil", err)
				}
				if message != tc.wantMessage {
					t.Errorf("tool_call_message = %q, 기대값 %q", message, tc.wantMessage)
				}
			})
		}
		for _, pc := range promptCases {
			t.Run(fmt.Sprintf("run%d/prompt/%s", run, pc.name), func(t *testing.T) {
				var gotNote sql.NullString
				var template string
				if err := pg.QueryRow(`SELECT note, template_text FROM agent_prompts WHERE id=$1`, promptIDs[pc.name]).Scan(&gotNote, &template); err != nil {
					t.Fatalf("조회 = %v, 기대값 nil", err)
				}
				if gotNote != pc.wantNote {
					t.Errorf("note = %+v, 기대값 %+v", gotNote, pc.wantNote)
				}
				if template != chineseTemplate {
					t.Errorf("template_text = %q, 기대값 %q(본문은 그대로)", template, chineseTemplate)
				}
			})
		}
		for _, lc := range linkCases {
			t.Run(fmt.Sprintf("run%d/link/%s", run, lc.name), func(t *testing.T) {
				var summary, source string
				if err := pg.QueryRow(`SELECT source_summary, source FROM task_asset_links WHERE task_id=$1 AND asset_id=$2`,
					task.ID, assetIDs[lc.name]).Scan(&summary, &source); err != nil {
					t.Fatalf("조회 = %v, 기대값 nil", err)
				}
				if summary != lc.wantSummary {
					t.Errorf("source_summary = %q, 기대값 %q", summary, lc.wantSummary)
				}
				if source != "manual" {
					t.Errorf("source = %q, 기대값 manual(출처 종류는 그대로)", source)
				}
			})
		}
	}
}

// TestRefreshLegacyChineseDefaultsWithoutTriggers 는 reporter 트리거가 하나도 없어도(사용자가 지움)
// 실패로 보지 않음을 확인한다. 실패로 보면 플래그가 켜지지 않아 시작할 때마다 다시 돈다.
func TestRefreshLegacyChineseDefaultsWithoutTriggers(t *testing.T) {
	pg := openReseedTestDB(t)
	agentKey := fmt.Sprintf("t170_no_trigger_%d", time.Now().UnixNano())
	if ok := refreshLegacyChineseDefaults(pg, agentKey); !ok {
		t.Fatalf("refreshLegacyChineseDefaults = false, 기대값 true")
	}
	triggers, err := pg.ListTriggersFor(agentKey)
	if err != nil {
		t.Fatalf("ListTriggersFor = %v, 기대값 nil", err)
	}
	if len(triggers) != 0 {
		t.Errorf("트리거 수 = %d, 기대값 0(새로 만들지 않음)", len(triggers))
	}
}

// TestRunMigrationOnce 는 플래그가 켜져 있으면 마이그레이션을 건너뛰고, 마이그레이션이 모두
// 성공했을 때만 플래그를 켬을 확인한다(#170).
func TestRunMigrationOnce(t *testing.T) {
	pg := openReseedTestDB(t)
	uniq := time.Now().UnixNano()
	tests := []struct {
		name         string
		initialFlag  string // 빈 문자열이면 설정 행을 만들지 않는다
		isSuccessful bool
		wantCalls    int
		wantFlag     string
	}{
		{name: "flag_on_skips", initialFlag: "true", isSuccessful: true, wantCalls: 0, wantFlag: "true"},
		{name: "success_sets_flag", initialFlag: "", isSuccessful: true, wantCalls: 1, wantFlag: "true"},
		{name: "failure_keeps_flag_off", initialFlag: "", isSuccessful: false, wantCalls: 1, wantFlag: ""},
		{name: "flag_false_runs", initialFlag: "false", isSuccessful: true, wantCalls: 1, wantFlag: "true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flag := fmt.Sprintf("t170_flag_%s_%d", tc.name, uniq)
			t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM settings WHERE key=$1`, flag) })
			if tc.initialFlag != "" {
				if err := pg.SetSetting(flag, tc.initialFlag); err != nil {
					t.Fatalf("SetSetting = %v, 기대값 nil", err)
				}
			}
			calls := 0
			runMigrationOnce(pg, flag, func() bool {
				calls++
				return tc.isSuccessful
			})
			if calls != tc.wantCalls {
				t.Errorf("마이그레이션 호출 수 = %d, 기대값 %d", calls, tc.wantCalls)
			}
			got, _, err := pg.GetSetting(flag)
			if err != nil {
				t.Fatalf("GetSetting = %v, 기대값 nil", err)
			}
			if got != tc.wantFlag {
				t.Errorf("플래그 = %q, 기대값 %q", got, tc.wantFlag)
			}
		})
	}
}
