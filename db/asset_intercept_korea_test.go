package db

import (
	"slices"
	"testing"
)

// assetRuleRow 는 비교에 쓰는 자산 차단 규칙 한 행이다.
type assetRuleRow struct {
	kind, pattern, note string
	isEnabled, builtin  bool
}

const (
	assetSeedV1Flag = "asset_intercept_default_rules_v1"
	assetSeedV2Flag = "asset_intercept_default_rules_v2_korea"
)

// wantDefaultAssetRules 는 새 설치와 이전을 마친 기존 설치가 가질 내장 규칙이다(#191 이슈 표).
var wantDefaultAssetRules = []assetRuleRow{
	{"fuzzy_domain", ".gov", "[내장] 미국 정부 웹사이트 (.gov)", true, true},
	{"fuzzy_domain", ".edu", "[내장] 미국 교육기관 웹사이트 (.edu)", true, true},
	{"fuzzy_domain", ".go.kr", "[내장] 한국 정부기관 웹사이트 (.go.kr)", true, true},
	{"fuzzy_domain", ".mil.kr", "[내장] 한국 국방기관 웹사이트 (.mil.kr)", true, true},
	{"fuzzy_domain", ".ac.kr", "[내장] 한국 대학 웹사이트 (.ac.kr)", true, true},
	{"fuzzy_domain", ".hs.kr", "[내장] 한국 고등학교·교육청 웹사이트 (.hs.kr)", true, true},
	{"fuzzy_domain", ".ms.kr", "[내장] 한국 중학교·교육청 웹사이트 (.ms.kr)", true, true},
	{"fuzzy_domain", ".es.kr", "[내장] 한국 초등학교·교육청 웹사이트 (.es.kr)", true, true},
	{"fuzzy_domain", ".sc.kr", "[내장] 한국 교육·훈련기관 웹사이트 (.sc.kr)", true, true},
	{"fuzzy_domain", ".kg.kr", "[내장] 한국 유치원·교육청 웹사이트 (.kg.kr)", true, true},
	{"exact_domain", "korea.kr", "[내장] 대한민국 정부 포털 (korea.kr)", true, true},
	{"fuzzy_domain", ".korea.kr", "[내장] 대한민국 정부 포털 하위 도메인 (.korea.kr)", true, true},
	{"exact_domain", "gov.kr", "[내장] 정부24 (gov.kr)", true, true},
	{"fuzzy_domain", ".gov.kr", "[내장] 정부24 하위 도메인 (.gov.kr)", true, true},
}

// v1SeedRows 는 #191 전의 v1 seed(번역 뒤 한국어 메모)가 남긴 행이다.
func v1SeedRows(isChineseEnabled bool) []assetRuleRow {
	return []assetRuleRow{
		{"fuzzy_domain", ".gov", "[내장] 정부 웹사이트 (.gov)", true, true},
		{"fuzzy_domain", ".gov.cn", "[내장] 정부 웹사이트 (.gov.cn)", isChineseEnabled, true},
		{"fuzzy_domain", ".edu", "[내장] 교육 웹사이트 (.edu)", true, true},
		{"fuzzy_domain", ".edu.cn", "[내장] 교육 웹사이트 (.edu.cn)", isChineseEnabled, true},
	}
}

// isolateAssetRules 는 공유 테스트 DB의 자산 차단 규칙과 seed 플래그를 비우고, 테스트가 끝나면 원래대로 돌린다.
func isolateAssetRules(t *testing.T, d *DB) {
	t.Helper()
	type savedRow struct {
		id int64
		assetRuleRow
	}
	rows, err := d.Query(`SELECT id, kind, pattern, note, enabled, builtin FROM asset_intercept_rules`)
	if err != nil {
		t.Fatal(err)
	}
	var saved []savedRow
	for rows.Next() {
		var r savedRow
		if err := rows.Scan(&r.id, &r.kind, &r.pattern, &r.note, &r.isEnabled, &r.builtin); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		saved = append(saved, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	savedFlags := map[string]string{}
	for _, k := range []string{assetSeedV1Flag, assetSeedV2Flag} {
		if v, ok, err := d.GetSetting(k); err != nil {
			t.Fatal(err)
		} else if ok {
			savedFlags[k] = v
		}
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM asset_intercept_rules`)
		for _, r := range saved {
			if _, err := d.Exec(`INSERT INTO asset_intercept_rules(id, kind, pattern, note, enabled, builtin) VALUES ($1,$2,$3,$4,$5,$6)`,
				r.id, r.kind, r.pattern, r.note, r.isEnabled, r.builtin); err != nil {
				t.Errorf("자산 차단 규칙 복원: %v", err)
			}
		}
		d.Exec(`DELETE FROM settings WHERE key IN ($1, $2)`, assetSeedV1Flag, assetSeedV2Flag)
		for k, v := range savedFlags {
			if err := d.SetSetting(k, v); err != nil {
				t.Errorf("설정 %s 복원: %v", k, err)
			}
		}
	})
	if _, err := d.Exec(`DELETE FROM asset_intercept_rules`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM settings WHERE key IN ($1, $2)`, assetSeedV1Flag, assetSeedV2Flag); err != nil {
		t.Fatal(err)
	}
}

func insertAssetRules(t *testing.T, d *DB, rules []assetRuleRow) {
	t.Helper()
	for _, r := range rules {
		if _, err := d.Exec(`INSERT INTO asset_intercept_rules(kind, pattern, note, enabled, builtin) VALUES ($1,$2,$3,$4,$5)`,
			r.kind, r.pattern, r.note, r.isEnabled, r.builtin); err != nil {
			t.Fatal(err)
		}
	}
}

// listAssetRules 는 모든 자산 차단 규칙을 정렬해 돌려준다. 중복 행도 그대로 담는다.
func listAssetRules(t *testing.T, d *DB) []assetRuleRow {
	t.Helper()
	rows, err := d.Query(`SELECT kind, pattern, note, enabled, builtin FROM asset_intercept_rules`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []assetRuleRow
	for rows.Next() {
		var r assetRuleRow
		if err := rows.Scan(&r.kind, &r.pattern, &r.note, &r.isEnabled, &r.builtin); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return sortedAssetRules(got)
}

func sortedAssetRules(rules []assetRuleRow) []assetRuleRow {
	out := slices.Clone(rules)
	slices.SortFunc(out, func(a, b assetRuleRow) int {
		if a.pattern != b.pattern {
			if a.pattern < b.pattern {
				return -1
			}
			return 1
		}
		if a.kind != b.kind {
			if a.kind < b.kind {
				return -1
			}
			return 1
		}
		if a.builtin != b.builtin {
			if a.builtin {
				return 1
			}
			return -1
		}
		return 0
	})
	return out
}

// withoutPattern 은 rules에서 kind·pattern이 같은 행을 뺀다.
func withoutPattern(rules []assetRuleRow, kind, pattern string) []assetRuleRow {
	return slices.DeleteFunc(slices.Clone(rules), func(r assetRuleRow) bool { return r.kind == kind && r.pattern == pattern })
}

// TestAssetInterceptKoreaMigration 은 새 설치와 기존 설치가 다시 시작한 뒤 중국 규칙 없이 미국·한국 내장 규칙을
// 하나씩만 갖는지, 사용자 규칙과 사용자가 지운 내장 규칙을 존중하는지 본다.
func TestAssetInterceptKoreaMigration(t *testing.T) {
	userGoKr := assetRuleRow{"fuzzy_domain", ".go.kr", "내가 만든 규칙", false, false}
	cases := []struct {
		name  string
		setup func(t *testing.T, d *DB)
		want  []assetRuleRow
	}{
		{
			name:  "새 설치",
			setup: func(t *testing.T, d *DB) {},
			want:  wantDefaultAssetRules,
		},
		{
			name: "v1 seed만 된 설치",
			setup: func(t *testing.T, d *DB) {
				insertAssetRules(t, d, v1SeedRows(true))
				if err := d.SetSetting(assetSeedV1Flag, "done"); err != nil {
					t.Fatal(err)
				}
			},
			want: wantDefaultAssetRules,
		},
		{
			name: "중국 규칙을 꺼 둔 설치",
			setup: func(t *testing.T, d *DB) {
				insertAssetRules(t, d, v1SeedRows(false))
				if err := d.SetSetting(assetSeedV1Flag, "done"); err != nil {
					t.Fatal(err)
				}
			},
			want: wantDefaultAssetRules,
		},
		{
			name: "사용자가 같은 패턴을 만든 설치",
			setup: func(t *testing.T, d *DB) {
				insertAssetRules(t, d, append(v1SeedRows(true), userGoKr))
				if err := d.SetSetting(assetSeedV1Flag, "done"); err != nil {
					t.Fatal(err)
				}
			},
			want: sortedAssetRules(append(withoutPattern(wantDefaultAssetRules, "fuzzy_domain", ".go.kr"), userGoKr)),
		},
		{
			// 트랜잭션 밖에서 일부만 반영된 상태(다른 버전이 남긴 것 등)에서도 중복 없이 마무리되는지 본다.
			name: "일부만 반영된 뒤 다시 시도",
			setup: func(t *testing.T, d *DB) {
				partial := withoutPattern(v1SeedRows(true), "fuzzy_domain", ".gov.cn")
				partial = append(partial, wantDefaultAssetRules[2], wantDefaultAssetRules[10])
				insertAssetRules(t, d, partial)
				if err := d.SetSetting(assetSeedV1Flag, "done"); err != nil {
					t.Fatal(err)
				}
			},
			want: wantDefaultAssetRules,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			isolateAssetRules(t, d)
			tc.setup(t, d)
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

// TestAssetInterceptKoreaMigrationKeepsDeletedBuiltin 은 이전을 마친 뒤 사용자가 지운 한국 내장 규칙이
// 다시 시작해도 되살아나지 않는지 본다.
func TestAssetInterceptKoreaMigrationKeepsDeletedBuiltin(t *testing.T) {
	d := openTestDB(t)
	isolateAssetRules(t, d)
	reopen(t)
	if _, err := d.Exec(`DELETE FROM asset_intercept_rules WHERE builtin AND pattern = '.ac.kr'`); err != nil {
		t.Fatal(err)
	}
	reopen(t)
	want := sortedAssetRules(withoutPattern(wantDefaultAssetRules, "fuzzy_domain", ".ac.kr"))
	if got := listAssetRules(t, d); !slices.Equal(got, want) {
		t.Fatalf("지운 뒤 다시 시작한 규칙\n got  %v\n want %v", got, want)
	}
}

// TestAssetInterceptUSNoteMigration 은 .gov·.edu 메모가 옛 seed 값과 같은 내장 행만 미국 기관 메모로 바뀌는지 본다.
func TestAssetInterceptUSNoteMigration(t *testing.T) {
	d := openTestDB(t)
	isolateAssetRules(t, d)
	if err := d.SetSetting(assetSeedV1Flag, "done"); err != nil {
		t.Fatal(err)
	}
	insertAssetRules(t, d, []assetRuleRow{
		{"fuzzy_domain", ".gov", "[内置] 政府网站 (.gov)", true, true},      // 번역 전 중국어 seed
		{"fuzzy_domain", ".edu", "[내장] 교육 웹사이트 (.edu)", false, true},  // 번역 뒤 seed, 꺼 둠
		{"exact_domain", "a.gov", "고친 메모", true, true},                // 사용자가 메모를 고친 내장 행
		{"exact_domain", "b.gov", "[내장] 정부 웹사이트 (.gov)", true, false}, // 옛 메모와 같은 사용자 규칙
	})
	reopen(t)
	got := listAssetRules(t, d)
	want := sortedAssetRules(append(slices.Clone(wantDefaultAssetRules[2:]),
		assetRuleRow{"fuzzy_domain", ".gov", "[내장] 미국 정부 웹사이트 (.gov)", true, true},
		assetRuleRow{"fuzzy_domain", ".edu", "[내장] 미국 교육기관 웹사이트 (.edu)", false, true},
		assetRuleRow{"exact_domain", "a.gov", "고친 메모", true, true},
		assetRuleRow{"exact_domain", "b.gov", "[내장] 정부 웹사이트 (.gov)", true, false},
	))
	if !slices.Equal(got, want) {
		t.Fatalf("메모 이전 뒤 규칙\n got  %v\n want %v", got, want)
	}
}

// TestAssetInterceptKoreaMigrationRetriesAfterFailure 는 이전이 중간에 실패하면 아무것도 바뀌지 않고 플래그도
// 켜지지 않으며, 다음 시작 때 처음부터 다시 돌아 중복 없이 끝나는지 본다.
func TestAssetInterceptKoreaMigrationRetriesAfterFailure(t *testing.T) {
	d := openTestDB(t)
	isolateAssetRules(t, d)
	insertAssetRules(t, d, v1SeedRows(true))
	if err := d.SetSetting(assetSeedV1Flag, "done"); err != nil {
		t.Fatal(err)
	}
	// 마지막에서 두 번째 한국 규칙을 넣을 때 실패하게 해, 앞의 지우기·넣기가 이미 실행된 뒤에 멈추게 한다.
	if _, err := d.Exec(`
CREATE OR REPLACE FUNCTION test_issue_191_fail_insert() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'test-issue-191 injected failure'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	dropTrigger := func() {
		d.Exec(`DROP TRIGGER IF EXISTS test_issue_191_fail ON asset_intercept_rules`)
		d.Exec(`DROP FUNCTION IF EXISTS test_issue_191_fail_insert()`)
	}
	t.Cleanup(dropTrigger)
	if _, err := d.Exec(`
CREATE TRIGGER test_issue_191_fail BEFORE INSERT ON asset_intercept_rules
FOR EACH ROW WHEN (NEW.pattern = 'gov.kr') EXECUTE FUNCTION test_issue_191_fail_insert()`); err != nil {
		t.Fatal(err)
	}

	failed, err := Open(testDSN(t))
	if err == nil {
		failed.Close()
		t.Fatal("넣기가 실패했는데 Open이 성공했다")
	}
	if got, want := listAssetRules(t, d), sortedAssetRules(v1SeedRows(true)); !slices.Equal(got, want) {
		t.Fatalf("실패한 이전 뒤 규칙이 바뀌었다\n got  %v\n want %v", got, want)
	}
	if v, ok, err := d.GetSetting(assetSeedV2Flag); err != nil || ok {
		t.Fatalf("실패한 이전 뒤 플래그 = (%q, %v, %v), 기대값 없음", v, ok, err)
	}

	dropTrigger()
	reopen(t)
	if got, want := listAssetRules(t, d), sortedAssetRules(wantDefaultAssetRules); !slices.Equal(got, want) {
		t.Fatalf("다시 시도한 뒤 규칙\n got  %v\n want %v", got, want)
	}
}
