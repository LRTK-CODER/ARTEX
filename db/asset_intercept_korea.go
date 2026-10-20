package db

import "fmt"

// assetInterceptKoreaFlag 는 기본 자산 차단 규칙을 한국 공공·교육기관 도메인으로 바꾸는 이전(#191)을
// 한 번만 돌리는 설정 키다. 켜진 뒤에는 사용자가 지운 한국 내장 규칙을 되살리지 않는다.
const assetInterceptKoreaFlag = "asset_intercept_default_rules_v2_korea"

// assetInterceptSeedRule 은 seed하는 내장 자산 차단 규칙 하나다.
type assetInterceptSeedRule struct{ kind, pattern, note string }

// koreanAssetInterceptRules 는 테스트 대상에서 빼는 한국 공공·교육기관 도메인이다(근거: #190 조사).
// korea.kr·gov.kr는 부분 일치로 넣으면 southkorea.kr·mygov.kr까지 막으므로 도메인 자체(정확히 일치)와
// 점을 붙인 하위 도메인(부분 일치) 두 줄로 넣는다.
var koreanAssetInterceptRules = []assetInterceptSeedRule{
	{"fuzzy_domain", ".go.kr", "[내장] 한국 정부기관 웹사이트 (.go.kr)"},
	{"fuzzy_domain", ".mil.kr", "[내장] 한국 국방기관 웹사이트 (.mil.kr)"},
	{"fuzzy_domain", ".ac.kr", "[내장] 한국 대학 웹사이트 (.ac.kr)"},
	{"fuzzy_domain", ".hs.kr", "[내장] 한국 고등학교·교육청 웹사이트 (.hs.kr)"},
	{"fuzzy_domain", ".ms.kr", "[내장] 한국 중학교·교육청 웹사이트 (.ms.kr)"},
	{"fuzzy_domain", ".es.kr", "[내장] 한국 초등학교·교육청 웹사이트 (.es.kr)"},
	{"fuzzy_domain", ".sc.kr", "[내장] 한국 교육·훈련기관 웹사이트 (.sc.kr)"},
	{"fuzzy_domain", ".kg.kr", "[내장] 한국 유치원·교육청 웹사이트 (.kg.kr)"},
	{"exact_domain", "korea.kr", "[내장] 대한민국 정부 포털 (korea.kr)"},
	{"fuzzy_domain", ".korea.kr", "[내장] 대한민국 정부 포털 하위 도메인 (.korea.kr)"},
	{"exact_domain", "gov.kr", "[내장] 정부24 (gov.kr)"},
	{"fuzzy_domain", ".gov.kr", "[내장] 정부24 하위 도메인 (.gov.kr)"},
}

// removedChineseAssetPatterns 는 v1 seed가 넣던 중국 규칙의 pattern이다(kind는 모두 fuzzy_domain).
// 소유자 결정(#100)으로 켜짐·꺼짐, 메모 수정 여부와 관계없이 내장 행을 지운다.
var removedChineseAssetPatterns = []string{".gov.cn", ".edu.cn"}

// migrateAssetInterceptRulesToKorea 는 내장 중국 규칙을 지우고 한국 규칙을 넣는다(한 번만).
// 새 설치도 v1 seed 뒤 이 이전으로 한국 규칙을 받는다. 표에 (kind, pattern) 유일 제약이 없어
// 같은 kind·pattern 행(사용자 규칙 포함)이 있으면 넣지 않는다. 플래그까지 한 트랜잭션이라
// 중간에 실패하면 아무것도 바뀌지 않고 다음 시작 때 처음부터 다시 돈다.
func (d *DB) migrateAssetInterceptRulesToKorea() error {
	v, _, err := d.GetSetting(assetInterceptKoreaFlag)
	if err != nil {
		return fmt.Errorf("%s 설정 읽기: %w", assetInterceptKoreaFlag, err)
	}
	if v == "done" {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	// Commit 뒤의 Rollback은 sql.ErrTxDone만 돌려주므로 버린다.
	defer func() { _ = tx.Rollback() }()
	for _, p := range removedChineseAssetPatterns {
		if _, err := tx.Exec(`DELETE FROM asset_intercept_rules WHERE builtin AND kind = 'fuzzy_domain' AND pattern = $1`, p); err != nil {
			return fmt.Errorf("중국 규칙 %q 지우기: %w", p, err)
		}
	}
	for _, r := range koreanAssetInterceptRules {
		if _, err := tx.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
SELECT true, $1::text, $2::text, $3, true
WHERE NOT EXISTS (
    SELECT 1 FROM asset_intercept_rules WHERE kind = $1::text AND lower(btrim(pattern)) = lower($2::text))`,
			r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("한국 규칙 %q 넣기: %w", r.pattern, err)
		}
	}
	if _, err := tx.Exec(`
INSERT INTO settings(key, value) VALUES ($1, 'done')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, assetInterceptKoreaFlag); err != nil {
		return fmt.Errorf("%s 설정 저장: %w", assetInterceptKoreaFlag, err)
	}
	return tx.Commit()
}
