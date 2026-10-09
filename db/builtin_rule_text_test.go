package db

import "testing"

// TestMigrateBuiltinRuleTexts 는 다시 시작할 때 이전 seed 값 그대로인 필드만 한국어로 바뀌고, 사용자가 고친 필드와
// 사용자 규칙은 그대로이며, 두 번 돌려도 결과가 같은지 본다.
func TestMigrateBuiltinRuleTexts(t *testing.T) {
	d := openTestDB(t)
	legacyName := legacyInterceptRuleNames[0]
	legacyMessage := legacyInterceptRuleMessages[0]
	legacyNote := legacyAssetInterceptRuleNotes[0]
	const userName = "사용자 규칙 이름"
	const userMessage = "사용자 규칙 메시지"
	const userNote = "사용자 메모"

	ruleCases := []struct {
		name                      string
		ruleName, message         string
		wantRuleName, wantMessage string
	}{
		{"기본값 그대로인 행", legacyName.legacy, legacyMessage.legacy, legacyName.current, legacyMessage.current},
		{"이름만 고친 행", userName, legacyMessage.legacy, userName, legacyMessage.current},
		{"메시지만 고친 행", legacyName.legacy, userMessage, legacyName.current, userMessage},
		{"사용자 규칙", userName, userMessage, userName, userMessage},
	}
	ruleIDs := make([]int64, len(ruleCases))
	for i, tc := range ruleCases {
		if err := d.QueryRow(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message)
VALUES ($1, false, 0, 'tool_input', 'string', 'test-issue-108', 'deny', $2) RETURNING id`,
			tc.ruleName, tc.message).Scan(&ruleIDs[i]); err != nil {
			t.Fatalf("차단 규칙 넣기(%s): %v", tc.name, err)
		}
		id := ruleIDs[i]
		t.Cleanup(func() { d.Exec(`DELETE FROM intercept_rules WHERE id=$1`, id) })
	}

	noteCases := []struct {
		name     string
		note     string
		builtin  bool
		wantNote string
	}{
		{"기본값 그대로인 내장 규칙", legacyNote.legacy, true, legacyNote.current},
		{"메모를 고친 내장 규칙", userNote, true, userNote},
		{"이전 값과 같은 메모의 사용자 규칙", legacyNote.legacy, false, legacyNote.legacy},
	}
	noteIDs := make([]int64, len(noteCases))
	for i, tc := range noteCases {
		if err := d.QueryRow(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (false, 'exact_domain', $1, $2, $3) RETURNING id`,
			"test-issue-108-"+tc.name, tc.note, tc.builtin).Scan(&noteIDs[i]); err != nil {
			t.Fatalf("자산 차단 규칙 넣기(%s): %v", tc.name, err)
		}
		id := noteIDs[i]
		t.Cleanup(func() { d.Exec(`DELETE FROM asset_intercept_rules WHERE id=$1`, id) })
	}

	for run := 1; run <= 2; run++ {
		reopen(t) // 다시 시작할 때마다 seed와 이전이 돈다
		for i, tc := range ruleCases {
			t.Run(tc.name, func(t *testing.T) {
				var gotName, gotMessage string
				if err := d.QueryRow(`SELECT name, message FROM intercept_rules WHERE id=$1`, ruleIDs[i]).Scan(&gotName, &gotMessage); err != nil {
					t.Fatal(err)
				}
				if gotName != tc.wantRuleName || gotMessage != tc.wantMessage {
					t.Fatalf("%d번째 이전 뒤 (이름, 메시지) = (%q, %q), 기대값 (%q, %q)", run, gotName, gotMessage, tc.wantRuleName, tc.wantMessage)
				}
			})
		}
		for i, tc := range noteCases {
			t.Run(tc.name, func(t *testing.T) {
				var gotNote string
				if err := d.QueryRow(`SELECT note FROM asset_intercept_rules WHERE id=$1`, noteIDs[i]).Scan(&gotNote); err != nil {
					t.Fatal(err)
				}
				if gotNote != tc.wantNote {
					t.Fatalf("%d번째 이전 뒤 메모 = %q, 기대값 %q", run, gotNote, tc.wantNote)
				}
			})
		}
	}
}
