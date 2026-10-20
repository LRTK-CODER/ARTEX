package server

import "testing"

// TestValidateAssetInterceptRuleReq 는 API가 suffix_domain을 받고, 점만 있는 패턴과 모르는 kind는 거부하는지 본다.
func TestValidateAssetInterceptRuleReq(t *testing.T) {
	cases := []struct {
		name, kind, pattern string
		isValid             bool
		wantPattern         string
	}{
		{"접미사 일치 앞 점 있음", "suffix_domain", " .go.kr ", true, ".go.kr"},
		{"접미사 일치 앞 점 없음", "suffix_domain", "go.kr", true, "go.kr"},
		{"접미사 일치 점만 있음", "suffix_domain", " . ", false, ""},
		{"접미사 일치 URL", "suffix_domain", "https://go.kr", false, ""},
		{"접미사 일치 경로", "suffix_domain", "go.kr/login", false, ""},
		{"접미사 일치 포트", "suffix_domain", "go.kr:443", false, ""},
		{"접미사 일치 가운데 공백", "suffix_domain", "go .kr", false, ""},
		{"접미사 일치 와일드카드", "suffix_domain", "*.go.kr", false, ""},
		{"접미사 일치 국제화 도메인", "suffix_domain", "정부.한국", true, "정부.한국"},
		{"접미사 일치 하이픈", "suffix_domain", "my-gov.kr", true, "my-gov.kr"},
		{"부분 일치 도메인", "fuzzy_domain", ".go.kr", true, ".go.kr"},
		{"모르는 kind", "suffix_url", "go.kr", false, ""},
		{"빈 pattern", "suffix_domain", "  ", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := assetInterceptRuleReq{Kind: tc.kind, Pattern: tc.pattern}
			err := validateAssetInterceptRuleReq(&req)
			if (err == nil) != tc.isValid {
				t.Fatalf("오류 = %v, 통과 기대 %v", err, tc.isValid)
			}
			if tc.isValid && req.Pattern != tc.wantPattern {
				t.Fatalf("pattern = %q, 기대값 %q", req.Pattern, tc.wantPattern)
			}
		})
	}
}

// TestValidateTaskInterceptRuleReqSuffix 는 작업 단위 규칙도 suffix_domain을 받는지 본다.
func TestValidateTaskInterceptRuleReqSuffix(t *testing.T) {
	req := taskInterceptRuleReq{Action: "allow", Kind: "suffix_domain", Pattern: "example.com"}
	if err := validateTaskInterceptRuleReq(&req); err != nil {
		t.Fatalf("작업 단위 suffix_domain 거부: %v", err)
	}
}
