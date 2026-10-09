package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 자산 차단 규칙의 일치 판정·실행 계층이다. asset_intercept.go는 규칙 저장만 맡고,
// 여기서는 '대상 자산'의 도메인/IP/URL을 사용 중인 규칙과 대조한다. 에이전트 도구
// (add_intent, insert_assets)가 의도를 내리거나 자산을 넣기 전에 부르고, 일치하면 거부한다.

// AssetInterceptKindLabel은 kind의 표시 라벨을 돌려준다. 에이전트에게 보내는 안내 메시지에 쓴다.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "도메인(정확히 일치)"
	case "exact_ip":
		return "IP(정확히 일치)"
	case "exact_url":
		return "URL(정확히 일치)"
	case "fuzzy_domain":
		return "도메인(부분 일치)"
	case "fuzzy_ip":
		return "IP(부분 일치)"
	case "fuzzy_url":
		return "URL(부분 일치)"
	case "cidr":
		return "CIDR 대역"
	}
	return kind
}

// Reason은 읽을 수 있는 일치 이유를 돌려준다. 형식: 자산 차단 규칙과 일치 [도메인(부분 일치): .gov.cn](메모).
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("자산 차단 규칙과 일치 [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "(" + note + ")"
	}
	return s
}

// matchOne은 사용 중인 규칙 하나가 주어진 도메인/IP/URL 후보 문자열과 일치하는지 판정하고, 일치한 값을 돌려준다.
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules는 주어진 도메인/IP/URL 후보 문자열과 처음 일치한 사용 중인 규칙과
// 일치한 값을 돌려준다. insert_assets가 원본 입력(아직 저장하지 않은 assetInputItem)을 대조할 때 쓴다.
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates는 저장된 자산에서 차단 판정에 쓸 도메인/IP/URL 후보 문자열을 뽑는다.
// URL의 host를 떼어 분류하므로 'URL만 있는' 서비스 자산도 도메인/IP 규칙과 일치할 수 있다.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel은 자산의 짧은 표시를 돌려준다. 에이전트에게 보내는 안내 메시지에 쓴다.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("자산#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule은 규칙 집합에 사용 중인 규칙이 하나라도 있는지 판정한다.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision은 '차단 먼저, 허용 다음' 검사가 후보 문자열 묶음에 내린 판정 결과다.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // 거부 이유(자산 표시 제외). Allowed=true이면 비어 있다
}

// EvaluateAssetGate는 작업 단위 검사를 판정한다.
//  1. 사용 중인 blockRules 중 하나와 일치하면 거부한다(차단 이유).
//  2. 아니면, 사용 중인 allowRules가 있는데 하나도 일치하지 않으면 거부한다(허용 범위 밖).
//  3. 그 밖에는 허용한다.
//
// allowRules가 비었거나 사용 중인 규칙이 없으면 허용 검사를 적용하지 않는다(허용 목록을
// 쓰지 않으므로 모두 허용). '허용 규칙을 설정하지 않음'이 모든 자산을 막지 않게 하려는 것이다.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "작업의 허용 목록 범위 밖이라 테스트할 수 없습니다"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit는 검사에서 거부된 자산 하나를 나타낸다(차단 규칙과 일치했거나 허용 범위 밖).
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // 읽을 수 있는 이유
}

// Describe는 읽을 수 있는 설명(자산 정보 + 이유)을 돌려준다.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules는 *DB의 같은 이름 메서드를 그대로 부른다. AssetStore만 가진
// 호출자(에이전트 도구 등)도 규칙을 읽을 수 있게 한다.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept는 id로 자산을 불러와 하나씩 '차단 먼저, 허용 다음' 검사를 판정하고,
// 거부된 자산을 모두 돌려준다. 차단 규칙 = 전역 ∪ 작업 단위 block, 허용 규칙 = 작업 단위
// allow(이 작업만). id가 없으면 바로 돌아간다. 작업 범위로 거르지 않는 전역 GetByIDs를 써서
// scope 때문에 차단이 약해지지 않게 한다.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// 차단 규칙도, 사용 중인 허용 규칙도 없으면 판정할 것이 없으므로 모두 허용한다.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
