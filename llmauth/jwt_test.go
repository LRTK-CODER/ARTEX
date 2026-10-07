package llmauth

import (
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
		{name: "점이 모자람", token: "abc.def", wantError: true},
		{name: "base64 가 아님", token: "a.!!!.c", wantError: true},
		{name: "JSON 이 아님", token: "a.bm90LWpzb24.c", wantError: true},
		{name: "exp 가 숫자가 아님", token: fakeJWT(t, map[string]any{"exp": "soon"}), wantError: true},
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
			if got.AccountID != tc.want.AccountID || !got.ExpiresAt.Equal(tc.want.ExpiresAt) {
				t.Fatalf("ParseClaims() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
