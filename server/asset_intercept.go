package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"unicode"

	"github.com/Autumn-27/artex/db"
)

// --- asset intercept rule CRUD ---
//
// 전역 자산 차단 목록: 도메인·IP·URL의 정확히 일치/부분 일치, 도메인 접미사 일치, CIDR 대역.
// 이 계층은 규칙 저장만 맡고, 일치 판정은 db/asset_intercept_match.go에 있다.

func (s *Server) assetInterceptListRules(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	rules, err := pg.ListAssetInterceptRules()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rules == nil {
		rules = []db.AssetInterceptRule{}
	}
	writeJSON(w, 200, map[string]any{"rules": rules})
}

func (s *Server) assetInterceptCreateRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req assetInterceptRuleReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateAssetInterceptRuleReq(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := pg.CreateAssetInterceptRule(req.Kind, req.Pattern, req.Note, req.Enabled)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rule)
}

func (s *Server) assetInterceptUpdateRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad rule id")
		return
	}
	var req assetInterceptRuleReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateAssetInterceptRuleReq(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := pg.UpdateAssetInterceptRule(id, req.Kind, req.Pattern, req.Note, req.Enabled)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rule)
}

func (s *Server) assetInterceptDeleteRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad rule id")
		return
	}
	if err := pg.DeleteAssetInterceptRule(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) assetInterceptToggleRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad rule id")
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.ToggleAssetInterceptRule(id, req.Enabled); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": req.Enabled})
}

// --- helpers ---

type assetInterceptRuleReq struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Note    string `json:"note"`
}

// isNonDomainRune 은 도메인 이름에 올 수 없는 문자(공백, URL·포트·와일드카드 구분자)인지 판정한다.
// 국제화 도메인을 막지 않으려고 허용 목록 대신 거부 목록으로 본다.
func isNonDomainRune(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(`/\:@?#*[]`, r)
}

// validateAssetInterceptRuleReq 는 규칙을 정규화하고 검사한다. pattern 앞뒤 공백을 지우고, 형식이 정해진
// kind(exact_ip·cidr)는 잘못된 값을 거부한다. suffix_domain은 점을 떼고 남는 도메인이 없거나 도메인에 올 수
// 없는 문자가 있으면 거부한다.
// 부분 일치 kind와 도메인·URL pattern은 자유 문자열로 둔다(해석은 일치 판정 계층이 한다).
func validateAssetInterceptRuleReq(req *assetInterceptRuleReq) error {
	req.Pattern = strings.TrimSpace(req.Pattern)
	if req.Pattern == "" {
		return fmt.Errorf("pattern은 비워 둘 수 없습니다")
	}
	switch req.Kind {
	case "exact_domain", "exact_url", "fuzzy_domain", "fuzzy_ip", "fuzzy_url":
		// 자유 문자열이라 형식을 검사하지 않는다
	case "suffix_domain":
		if db.NormalizeSuffixDomain(req.Pattern) == "" {
			return fmt.Errorf("suffix_domain에 도메인이 없습니다(예: go.kr): %s", req.Pattern)
		}
		// URL이나 host:port를 넣으면 어떤 도메인 후보와도 맞지 않는 규칙이 조용히 생기므로 미리 거부한다.
		if strings.IndexFunc(req.Pattern, isNonDomainRune) >= 0 {
			return fmt.Errorf("suffix_domain에는 도메인만 넣습니다(/·:·공백 등 불가, 예: go.kr): %s", req.Pattern)
		}
	case "exact_ip":
		if net.ParseIP(req.Pattern) == nil {
			return fmt.Errorf("exact_ip에 잘못된 IP 주소: %s", req.Pattern)
		}
	case "cidr":
		if _, _, err := net.ParseCIDR(req.Pattern); err != nil {
			return fmt.Errorf("cidr에 잘못된 네트워크 대역(예: 192.168.0.0/16): %s", req.Pattern)
		}
	default:
		return fmt.Errorf("잘못된 kind: %s", req.Kind)
	}
	return nil
}
