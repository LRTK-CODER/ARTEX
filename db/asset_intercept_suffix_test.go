package db

import (
	"slices"
	"testing"
)

// setAssetSeedFlags 는 v1·v2 seed를 마친 설치처럼 플래그를 켠다.
func setAssetSeedFlags(t *testing.T, d *DB) {
	t.Helper()
	for _, k := range []string{assetSeedV1Flag, assetSeedV2Flag} {
		if err := d.SetSetting(k, "done"); err != nil {
			t.Fatal(err)
		}
	}
}

// replacePattern 은 rules에서 kind·pattern이 같은 행을 row로 바꾼다.
func replacePattern(rules []assetRuleRow, kind, pattern string, row assetRuleRow) []assetRuleRow {
	out := slices.Clone(rules)
	for i, r := range out {
		if r.kind == kind && r.pattern == pattern {
			out[i] = row
		}
	}
	return out
}

// TestAssetInterceptSuffixMigration 은 v2까지 마친 설치의 내장 규칙이 suffix_domain으로 한 번 옮겨지고,
// 사용자가 kind·pattern을 고친 행, 사용자 규칙, 한쪽만 지우거나 끈 두 줄 규칙은 그대로 남는지 본다(#193).
func TestAssetInterceptSuffixMigration(t *testing.T) {
	editedMil := assetRuleRow{"fuzzy_domain", ".mil.kr.example", "[내장] 한국 국방기관 웹사이트 (.mil.kr)", true, true}
	userGoKr := assetRuleRow{"fuzzy_domain", ".go.kr", "내가 만든 규칙", true, false}
	userEdu := assetRuleRow{"suffix_domain", "EDU", "내 접미사 규칙", true, false}
	exactKoreaOff := assetRuleRow{"exact_domain", "korea.kr", "[내장] 대한민국 정부 포털 (korea.kr)", false, true}

	userEdited := v2DefaultAssetRules
	userEdited = replacePattern(userEdited, "fuzzy_domain", ".mil.kr", editedMil)
	userEdited = replacePattern(userEdited, "fuzzy_domain", ".ac.kr", assetRuleRow{"fuzzy_domain", ".ac.kr", "[내장] 한국 대학 웹사이트 (.ac.kr)", false, true})
	userEdited = replacePattern(userEdited, "fuzzy_domain", ".hs.kr", assetRuleRow{"fuzzy_domain", ".hs.kr", "내 메모", true, true})
	userEdited = replacePattern(userEdited, "exact_domain", "korea.kr", exactKoreaOff)
	userEdited = withoutPattern(userEdited, "exact_domain", "gov.kr")
	userEdited = append(userEdited, userGoKr, userEdu)

	userEditedWant := wantDefaultAssetRules
	userEditedWant = replacePattern(userEditedWant, "suffix_domain", ".mil.kr", editedMil)
	userEditedWant = replacePattern(userEditedWant, "suffix_domain", ".ac.kr", assetRuleRow{"suffix_domain", ".ac.kr", "[내장] 한국 대학 웹사이트 (.ac.kr)", false, true})
	userEditedWant = replacePattern(userEditedWant, "suffix_domain", ".hs.kr", assetRuleRow{"suffix_domain", ".hs.kr", "내 메모", true, true})
	userEditedWant = replacePattern(userEditedWant, "suffix_domain", ".edu", v2DefaultAssetRules[1]) // 같은 접미사 규칙이 있어 옮기지 않음
	userEditedWant = withoutPattern(userEditedWant, "suffix_domain", "korea.kr")
	userEditedWant = withoutPattern(userEditedWant, "suffix_domain", "gov.kr")
	userEditedWant = append(userEditedWant, exactKoreaOff, v2DefaultAssetRules[11], v2DefaultAssetRules[13], userGoKr, userEdu)

	bothOff := replacePattern(v2DefaultAssetRules, "exact_domain", "korea.kr", exactKoreaOff)
	bothOff = replacePattern(bothOff, "fuzzy_domain", ".korea.kr", assetRuleRow{"fuzzy_domain", ".korea.kr", "[내장] 대한민국 정부 포털 하위 도메인 (.korea.kr)", false, true})
	bothOffWant := replacePattern(wantDefaultAssetRules, "suffix_domain", "korea.kr", assetRuleRow{"suffix_domain", "korea.kr", "[내장] 대한민국 정부 포털과 하위 도메인 (korea.kr)", false, true})

	cases := []struct {
		name       string
		rows, want []assetRuleRow
	}{
		{"v2까지 마친 설치", v2DefaultAssetRules, wantDefaultAssetRules},
		{"사용자가 고친 행과 사용자 규칙", userEdited, userEditedWant},
		{"두 줄을 모두 꺼 둔 설치", bothOff, bothOffWant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			isolateAssetRules(t, d)
			setAssetSeedFlags(t, d)
			insertAssetRules(t, d, tc.rows)
			want := sortedAssetRules(tc.want)
			for run := 1; run <= 2; run++ {
				reopen(t) // 다시 시작할 때마다 seed와 이전이 돈다
				if got := listAssetRules(t, d); !slices.Equal(got, want) {
					t.Fatalf("%d번째 시작 뒤 규칙\n got  %v\n want %v", run, got, want)
				}
			}
		})
	}
}

// TestAssetInterceptSuffixMigrationRetriesAfterFailure 는 접미사 일치 이전이 중간에 실패하면 아무것도
// 바뀌지 않고 플래그도 켜지지 않으며, 다음 시작 때 다시 돌아 중복 없이 끝나는지 본다.
func TestAssetInterceptSuffixMigrationRetriesAfterFailure(t *testing.T) {
	d := openTestDB(t)
	isolateAssetRules(t, d)
	setAssetSeedFlags(t, d)
	insertAssetRules(t, d, v2DefaultAssetRules)
	// 마지막 합치기에서 exact gov.kr 행을 지울 때 실패하게 해, 앞의 바꾸기가 이미 실행된 뒤에 멈추게 한다.
	if _, err := d.Exec(`
CREATE OR REPLACE FUNCTION test_issue_193_fail_delete() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'test-issue-193 injected failure'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	dropTrigger := func() {
		if _, err := d.Exec(`DROP TRIGGER IF EXISTS test_issue_193_fail ON asset_intercept_rules`); err != nil {
			t.Errorf("시험 트리거 지우기: %v", err)
		}
		if _, err := d.Exec(`DROP FUNCTION IF EXISTS test_issue_193_fail_delete()`); err != nil {
			t.Errorf("시험 함수 지우기: %v", err)
		}
	}
	t.Cleanup(dropTrigger)
	if _, err := d.Exec(`
CREATE TRIGGER test_issue_193_fail BEFORE DELETE ON asset_intercept_rules
FOR EACH ROW WHEN (OLD.pattern = 'gov.kr') EXECUTE FUNCTION test_issue_193_fail_delete()`); err != nil {
		t.Fatal(err)
	}

	failed, err := Open(testDSN(t))
	if err == nil {
		failed.Close()
		t.Fatal("지우기가 실패했는데 Open이 성공했다")
	}
	if got, want := listAssetRules(t, d), sortedAssetRules(v2DefaultAssetRules); !slices.Equal(got, want) {
		t.Fatalf("실패한 이전 뒤 규칙이 바뀌었다\n got  %v\n want %v", got, want)
	}
	if v, ok, err := d.GetSetting(assetSeedV3Flag); err != nil || ok {
		t.Fatalf("실패한 이전 뒤 플래그 = (%q, %v, %v), 기대값 없음", v, ok, err)
	}

	dropTrigger()
	reopen(t)
	if got, want := listAssetRules(t, d), sortedAssetRules(wantDefaultAssetRules); !slices.Equal(got, want) {
		t.Fatalf("다시 시도한 뒤 규칙\n got  %v\n want %v", got, want)
	}
}
