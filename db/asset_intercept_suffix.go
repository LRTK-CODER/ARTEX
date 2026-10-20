package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// assetInterceptSuffixFlag 는 내장 자산 차단 규칙을 suffix_domain으로 옮기는 이전(#193)을 한 번만 돌리는 설정 키다.
const assetInterceptSuffixFlag = "asset_intercept_default_rules_v3_suffix"

// builtinFuzzyToSuffixPatterns 는 kind만 fuzzy_domain에서 suffix_domain으로 바꾸는 내장 규칙의 pattern이다.
// suffix_domain은 앞 점을 무시하므로 pattern과 메모는 그대로 둔다.
var builtinFuzzyToSuffixPatterns = []string{
	".gov", ".edu",
	".go.kr", ".mil.kr", ".ac.kr", ".hs.kr", ".ms.kr", ".es.kr", ".sc.kr", ".kg.kr",
}

// builtinSuffixMerge 는 v2가 도메인 자체(exact_domain)와 하위 도메인(fuzzy_domain) 두 줄로 넣은 규칙을
// suffix_domain 한 줄로 합치는 정보다.
type builtinSuffixMerge struct {
	exactPattern, fuzzyPattern string
	suffixPattern, note        string
}

var builtinSuffixMerges = []builtinSuffixMerge{
	{"korea.kr", ".korea.kr", "korea.kr", "[내장] 대한민국 정부 포털과 하위 도메인 (korea.kr)"},
	{"gov.kr", ".gov.kr", "gov.kr", "[내장] 정부24와 하위 도메인 (gov.kr)"},
}

// migrateAssetInterceptRulesToSuffix 는 사용자가 kind·pattern을 고치지 않은 내장 규칙을 suffix_domain으로
// 옮긴다(한 번만). 새 설치도 v1·v2 seed 뒤 이 이전을 타서 기존 설치와 같은 규칙을 갖는다.
//
// 사용자의 뜻을 바꾸지 않으려고 아래 경우는 건드리지 않는다.
//   - 같은 suffix 패턴의 suffix_domain 행이 이미 있으면(사용자 규칙 포함) 옮기지 않는다(중복 방지).
//   - 두 줄 합치기는 내장 두 행이 모두 있고 켜짐 상태가 같을 때만 한다. 한 줄을 지웠거나 한 줄만 껐으면
//     사용자가 도메인 자체나 하위 도메인 중 하나만 풀어 둔 것이라 합치면 그 선택이 사라진다.
//
// 같은 pattern의 내장 fuzzy 행이 이미 여럿이면(손으로 복제한 경우) 모두 바꾼다. NOT EXISTS는 문장 시작
// 때의 스냅숏을 보므로 이전 전부터 있던 suffix 행만 막는다. 하나만 바꾸면 남은 부분 일치 행이 이 이전이
// 없애려는 과잉 차단을 계속하고, 모두 바꾸면 이전 전과 같은 수의 행이 같은 뜻을 지킨다. 두 줄 합치기는
// 가장 앞 행(id 순) 한 쌍만 합치고 나머지 복제 행은 그대로 둔다.
//
// 시작 시 seedBuiltins는 스키마 advisory lock 안에서 돌고, 플래그까지 한 트랜잭션이라 중간에 실패하면
// 아무것도 바뀌지 않고 다음 시작 때 처음부터 다시 돈다.
func (d *DB) migrateAssetInterceptRulesToSuffix() error {
	v, _, err := d.GetSetting(assetInterceptSuffixFlag)
	if err != nil {
		return fmt.Errorf("%s 설정 읽기: %w", assetInterceptSuffixFlag, err)
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
	for _, p := range builtinFuzzyToSuffixPatterns {
		if _, err := tx.Exec(`
UPDATE asset_intercept_rules SET kind = 'suffix_domain'
WHERE builtin AND kind = 'fuzzy_domain' AND pattern = $1
  AND NOT EXISTS (
    SELECT 1 FROM asset_intercept_rules
    WHERE kind = 'suffix_domain' AND btrim(lower(btrim(pattern)), '.') = btrim(lower($1::text), '.'))`, p); err != nil {
			return fmt.Errorf("내장 규칙 %q 접미사 일치로 바꾸기: %w", p, err)
		}
	}
	for _, m := range builtinSuffixMerges {
		if err := mergeBuiltinSuffixRule(tx, m); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
INSERT INTO settings(key, value) VALUES ($1, 'done')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, assetInterceptSuffixFlag); err != nil {
		return fmt.Errorf("%s 설정 저장: %w", assetInterceptSuffixFlag, err)
	}
	return tx.Commit()
}

// mergeBuiltinSuffixRule 은 내장 exact·fuzzy 두 행을 suffix_domain 한 행으로 합친다. fuzzy 행을 바꾸고
// exact 행을 지운다. 합칠 조건(migrateAssetInterceptRulesToSuffix 주석)이 맞지 않으면 그대로 둔다.
func mergeBuiltinSuffixRule(tx *sql.Tx, m builtinSuffixMerge) error {
	var exactID, fuzzyID int64
	var isExactEnabled, isFuzzyEnabled bool
	err := tx.QueryRow(`SELECT id, enabled FROM asset_intercept_rules WHERE builtin AND kind = 'exact_domain' AND pattern = $1 ORDER BY id LIMIT 1`,
		m.exactPattern).Scan(&exactID, &isExactEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("내장 규칙 %q 읽기: %w", m.exactPattern, err)
	}
	err = tx.QueryRow(`SELECT id, enabled FROM asset_intercept_rules WHERE builtin AND kind = 'fuzzy_domain' AND pattern = $1 ORDER BY id LIMIT 1`,
		m.fuzzyPattern).Scan(&fuzzyID, &isFuzzyEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("내장 규칙 %q 읽기: %w", m.fuzzyPattern, err)
	}
	if isExactEnabled != isFuzzyEnabled {
		return nil
	}
	var hasSuffix bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM asset_intercept_rules WHERE kind = 'suffix_domain' AND btrim(lower(btrim(pattern)), '.') = $1)`,
		NormalizeSuffixDomain(m.suffixPattern)).Scan(&hasSuffix); err != nil {
		return fmt.Errorf("접미사 규칙 %q 찾기: %w", m.suffixPattern, err)
	}
	if hasSuffix {
		return nil
	}
	if _, err := tx.Exec(`UPDATE asset_intercept_rules SET kind = 'suffix_domain', pattern = $2, note = $3 WHERE id = $1`,
		fuzzyID, m.suffixPattern, m.note); err != nil {
		return fmt.Errorf("내장 규칙 %q 합치기: %w", m.suffixPattern, err)
	}
	if _, err := tx.Exec(`DELETE FROM asset_intercept_rules WHERE id = $1`, exactID); err != nil {
		return fmt.Errorf("내장 규칙 %q 지우기: %w", m.exactPattern, err)
	}
	return nil
}
