package server

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

// TestRefreshHanToolDefaults 는 한자가 남은 설명·schema만 코드 기본값으로 바뀌고,
// 사용자가 한국어·영어로 고친 값과 연결·사용 여부는 그대로이며, 두 번 돌려도 같음을 확인한다(#145).
func TestRefreshHanToolDefaults(t *testing.T) {
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

	uniq := time.Now().UnixNano()
	key := func(name string) string { return fmt.Sprintf("t145_%s_%d", name, uniq) }

	const (
		englishDesc     = "List the facts recorded for the current task."
		englishSchema   = `{"type":"object","properties":{"limit":{"type":"integer","description":"Maximum rows to return."}}}`
		chineseDesc     = "列出当前任务已记录的事实。"
		chineseSchema   = `{"type":"object","properties":{"limit":{"type":"integer","description":"最多返回条数。"}}}`
		koreanDesc      = "현재 작업에 기록된 사실을 보여 준다."
		editedEnDesc    = "My own description, keep it."
		editedEnSchema  = `{"type":"object","properties":{"limit":{"type":"integer","description":"How many.","default":5}}}`
		customToolDescZ = "用户自定义工具"
		// 코드 기본값이 일부러 한자를 담는 경우(traffic_search의 중국어 검색 예시).
		exampleSchema       = `{"type":"object","properties":{"q":{"type":"string","description":"Body search, e.g. '内网测试'"}}}`
		editedExampleSchema = `{"type":"object","properties":{"q":{"type":"string","description":"My search, e.g. '内网测试'","default":"root"}}}`
		legacyExampleSchema = `{"type":"object","properties":{"q":{"type":"string","description":"正文全文搜索，例如 '内网测试'"}}}`
	)

	type row struct {
		name, desc, schema string
		isSystem           bool
	}
	rows := []row{
		{"zh_both", chineseDesc, chineseSchema, true},       // 옛 중국어 기본값 그대로
		{"zh_desc_only", chineseDesc, editedEnSchema, true}, // 설명만 중국어, schema는 사용자가 고침
		{"ko_desc_zh_schema", koreanDesc, chineseSchema, true},
		{"en_edited", editedEnDesc, editedEnSchema, true},           // 사용자가 영어로 고침
		{"custom_zh", customToolDescZ, chineseSchema, false},        // 사용자 지정 도구(system=false)
		{"already_en", englishDesc, englishSchema, true},            // 이미 영어(새 설치)
		{"no_default_zh", chineseDesc, chineseSchema, true},         // 코드 기본값이 없는 행
		{"example_edited", editedEnDesc, editedExampleSchema, true}, // 기본값에도 있는 한자만 남은 사용자 수정 schema
		{"example_legacy", englishDesc, legacyExampleSchema, true},  // 기본값에 없는 옛 한자가 남은 schema
	}
	for _, r := range rows {
		k := key(r.name)
		if _, err := pg.Exec(`INSERT INTO tools(key, system, description, schema, agents, enabled) VALUES ($1,$2,$3,$4,'["reporter"]',false)`,
			k, r.isSystem, r.desc, r.schema); err != nil {
			t.Fatalf("INSERT %s = %v, 기대값 nil", k, err)
		}
		t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM tools WHERE key=$1`, k) })
	}

	var defaults []toolDefault
	for _, r := range rows {
		if r.name == "no_default_zh" {
			continue
		}
		schema := englishSchema
		if r.name == "example_edited" || r.name == "example_legacy" {
			schema = exampleSchema
		}
		defaults = append(defaults, toolDefault{key: key(r.name), desc: englishDesc, schema: json.RawMessage(schema)})
	}

	want := map[string]struct{ desc, schema string }{
		"zh_both":           {englishDesc, englishSchema},
		"zh_desc_only":      {englishDesc, editedEnSchema},
		"ko_desc_zh_schema": {koreanDesc, englishSchema},
		"en_edited":         {editedEnDesc, editedEnSchema},
		"custom_zh":         {customToolDescZ, chineseSchema},
		"already_en":        {englishDesc, englishSchema},
		"no_default_zh":     {chineseDesc, chineseSchema},
		"example_edited":    {editedEnDesc, editedExampleSchema},
		"example_legacy":    {englishDesc, exampleSchema},
	}

	for run := 1; run <= 2; run++ { // 두 번째 실행은 멱등 확인이다
		if ok := refreshHanToolDefaults(pg, defaults); !ok {
			t.Fatalf("%d번째 refreshHanToolDefaults = false, 기대값 true", run)
		}
		for _, r := range rows {
			t.Run(fmt.Sprintf("run%d/%s", run, r.name), func(t *testing.T) {
				var desc string
				var schema json.RawMessage
				var agents string
				var isEnabled bool
				if err := pg.QueryRow(`SELECT description, schema, agents::text, enabled FROM tools WHERE key=$1`, key(r.name)).
					Scan(&desc, &schema, &agents, &isEnabled); err != nil {
					t.Fatalf("조회 = %v, 기대값 nil", err)
				}
				w := want[r.name]
				if desc != w.desc {
					t.Errorf("description = %q, 기대값 %q", desc, w.desc)
				}
				if !jsonEqual(t, schema, json.RawMessage(w.schema)) {
					t.Errorf("schema = %s, 기대값 %s", schema, w.schema)
				}
				if agents != `["reporter"]` || isEnabled {
					t.Errorf("agents=%s enabled=%v, 기대값 [\"reporter\"] false(연결·사용 여부 보존)", agents, isEnabled)
				}
			})
		}
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("json.Unmarshal(%s) = %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("json.Unmarshal(%s) = %v", b, err)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}
