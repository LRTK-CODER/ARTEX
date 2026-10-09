package llmauth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseClaims(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name      string
		token     string
		want      Claims
		wantError bool
	}{
		{
			name:  "계정과 만료",
			token: accessJWT(t, "acct-1", exp),
			want:  Claims{AccountID: "acct-1", ExpiresAt: exp},
		},
		{
			name:  "계정 클레임 없음",
			token: fakeJWT(t, map[string]any{"exp": exp.Unix()}),
			want:  Claims{ExpiresAt: exp},
		},
		{
			name:  "만료 클레임 없음",
			token: fakeJWT(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-2"}}),
			want:  Claims{AccountID: "acct-2"},
		},
		{
			name:  "패딩 붙은 payload",
			token: "e30." + strings.Split(accessJWT(t, "acct-3", exp), ".")[1] + "==.sig",
			want:  Claims{AccountID: "acct-3", ExpiresAt: exp},
		},
		{
			name:  "플랜은 소문자로",
			token: fakeJWT(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "Plus"}}),
			want:  Claims{PlanType: "plus"},
		},
		{
			name:  "플랜이 문자열이 아니면 빈 값",
			token: fakeJWT(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-4", "chatgpt_plan_type": 3}}),
			want:  Claims{AccountID: "acct-4"},
		},
		{
			name:  "플랜 이름 모양이 아니면 빈 값",
			token: fakeJWT(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "<b>pro</b>"}}),
			want:  Claims{},
		},
		{name: "점이 모자람", token: "abc.def", wantError: true},
		{name: "base64가 아님", token: "a.!!!.c", wantError: true},
		{name: "JSON이 아님", token: "a.bm90LWpzb24.c", wantError: true},
		{name: "exp가 숫자가 아님", token: fakeJWT(t, map[string]any{"exp": "soon"}), wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseClaims(tc.token)
			if tc.wantError {
				if err == nil {
					t.Fatalf("ParseClaims() = %+v, want error", got)
				}
				if strings.Contains(err.Error(), tc.token) {
					t.Fatalf("error %q contains token", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseClaims() err = %v", err)
			}
			if got.AccountID != tc.want.AccountID || !got.ExpiresAt.Equal(tc.want.ExpiresAt) || got.PlanType != tc.want.PlanType {
				t.Fatalf("ParseClaims() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestTokensPlanType 은 토큰 교환 결과의 플랜이 id token을 앞세우고, 없으면 access token 에서 오는지 본다.
func TestTokensPlanType(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name       string
		idPlan     string
		accessPlan string
		wantPlan   string
	}{
		{name: "id token의 플랜", idPlan: "plus", wantPlan: "plus"},
		{name: "access token 만 플랜", accessPlan: "pro", wantPlan: "pro"},
		{name: "둘 다 있으면 id token", idPlan: "plus", accessPlan: "pro", wantPlan: "plus"},
		{name: "플랜 클레임 없음", wantPlan: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAuthServer(t, now)
			f.idTokenPlan, f.accessTokenPlan = tc.idPlan, tc.accessPlan
			tokens, err := f.client().ExchangeCode(context.Background(), "code-1", "verifier-1", RedirectURI)
			if err != nil {
				t.Fatalf("ExchangeCode: %v", err)
			}
			if tokens.PlanType != tc.wantPlan {
				t.Fatalf("PlanType = %q, want %q", tokens.PlanType, tc.wantPlan)
			}
		})
	}
}
