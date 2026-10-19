package server

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// TestRefreshHanAgentDefaults 는 번역 전 설치에 남은 사용자 지정 에이전트의 중국어 이름·설명만
// 코드의 한국어 기본값으로 바뀌고, 사용자가 한국어·영어로 고친 값, 내장 에이전트, 지운 행은
// 그대로이며, 두 번 돌려도 같음을 확인한다(#160).
func TestRefreshHanAgentDefaults(t *testing.T) {
	skipWithoutPostgres(t)
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres not configured: %v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	uniq := time.Now().UnixNano()
	key := func(name string) string { return fmt.Sprintf("t160_%s_%d", name, uniq) }

	// 운영 DB에서 조회한 옛 중국어 값(이슈 #160)과 코드의 한국어 기본값이다.
	const (
		chineseReporterName = "报告撰写"
		chineseReporterDesc = "漏洞详细报告撰写：发现漏洞时自动触发，查取证据与执行过程后写 Markdown 报告并回写。"
		chineseRetesterName = "漏洞复测"
		chineseRetesterDesc = "从漏洞详情手动启动，读取原证据并保存独立复测结论。"
		koreanEditedName    = "내 보고서 봇"
		koreanEditedDesc    = "내가 고친 설명이다."
		englishEditedName   = "My reporter"
		englishEditedDesc   = "My own description, keep it."
	)

	type row struct {
		name, agentName, desc string
		isBuiltin             bool
	}
	rows := []row{
		{"zh_reporter", chineseReporterName, chineseReporterDesc, false},   // 번역 전 설치 그대로
		{"zh_retester", chineseRetesterName, chineseRetesterDesc, false},   // 번역 전 설치 그대로
		{"zh_name_only", chineseReporterName, koreanEditedDesc, false},     // 설명만 사용자가 고침
		{"ko_edited", koreanEditedName, koreanEditedDesc, false},           // 사용자가 한국어로 고침
		{"en_edited", englishEditedName, englishEditedDesc, false},         // 사용자가 영어로 고침
		{"already_ko", reporterAgentName, reporterAgentDescription, false}, // 새 설치
		{"zh_builtin", chineseReporterName, chineseReporterDesc, true},     // 내장 에이전트는 이 마이그레이션 대상이 아니다
	}
	for _, r := range rows {
		k := key(r.name)
		if _, err := pg.Exec(`INSERT INTO agents(key, name, description, role, builtin, enabled) VALUES ($1,$2,$3,'assistant',$4,false)`,
			k, r.agentName, r.desc, r.isBuiltin); err != nil {
			t.Fatalf("INSERT %s = %v, 기대값 nil", k, err)
		}
		t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM agents WHERE key=$1`, k) })
	}

	defaults := []agentDefault{{key: key("deleted"), name: reporterAgentName, description: reporterAgentDescription}}
	for _, r := range rows {
		d := agentDefault{key: key(r.name), name: reporterAgentName, description: reporterAgentDescription}
		if r.name == "zh_retester" {
			d.name, d.description = retesterAgentName, retesterAgentDescription
		}
		defaults = append(defaults, d)
	}

	want := map[string]struct{ name, desc string }{
		"zh_reporter":  {reporterAgentName, reporterAgentDescription},
		"zh_retester":  {retesterAgentName, retesterAgentDescription},
		"zh_name_only": {reporterAgentName, koreanEditedDesc},
		"ko_edited":    {koreanEditedName, koreanEditedDesc},
		"en_edited":    {englishEditedName, englishEditedDesc},
		"already_ko":   {reporterAgentName, reporterAgentDescription},
		"zh_builtin":   {chineseReporterName, chineseReporterDesc},
	}

	for run := 1; run <= 2; run++ { // 두 번째 실행은 멱등 확인이다
		if ok := refreshHanAgentDefaults(pg, defaults); !ok {
			t.Fatalf("%d번째 refreshHanAgentDefaults = false, 기대값 true", run)
		}
		for _, r := range rows {
			t.Run(fmt.Sprintf("run%d/%s", run, r.name), func(t *testing.T) {
				var name, desc string
				var isEnabled bool
				if err := pg.QueryRow(`SELECT name, description, enabled FROM agents WHERE key=$1`, key(r.name)).
					Scan(&name, &desc, &isEnabled); err != nil {
					t.Fatalf("조회 = %v, 기대값 nil", err)
				}
				w := want[r.name]
				if name != w.name || desc != w.desc {
					t.Errorf("name, description = %q, %q, 기대값 %q, %q", name, desc, w.name, w.desc)
				}
				if isEnabled {
					t.Errorf("enabled = true, 기대값 false(사용 여부 보존)")
				}
			})
		}
		t.Run(fmt.Sprintf("run%d/deleted", run), func(t *testing.T) {
			var id int64
			err := pg.QueryRow(`SELECT id FROM agents WHERE key=$1`, key("deleted")).Scan(&id)
			if !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("지운 에이전트 조회 오류 = %v, 기대값 sql.ErrNoRows(다시 만들지 않음)", err)
			}
		})
	}
}
