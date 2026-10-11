package agent

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// TestJudgeTemperatureByAuthType 은 승인 심사처럼 Temperature=0을 실은 요청이 프로필 인증 방식에 따라
// 어떤 본문으로 나가는지 확인한다(#133). Codex 백엔드는 temperature를 400으로 거부하므로 구독 경로에서는
// 빠져야 하고, API 키 경로는 그대로 0을 보내 결정적인 심사를 유지해야 한다.
func TestJudgeTemperatureByAuthType(t *testing.T) {
	cases := []struct {
		name            string
		newProvider     func(t *testing.T, baseURL string) llm.Provider
		wantTemperature bool
	}{
		{"chatgpt oauth", func(t *testing.T, baseURL string) llm.Provider {
			return newOAuthProvider(t, baseURL, &fakeTokens{accessTokens: []string{fakeAccessToken}})
		}, false},
		{"api key", func(t *testing.T, baseURL string) llm.Provider {
			c := ConfigFrom("openai-responses", "gpt-5", baseURL, "sk-test-apikey", "")
			c.Retry.ConnectAttempts = -1
			prov, err := c.NewProvider()
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			return prov
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &codexServer{sseBody: textTurnSSE}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			prov := tc.newProvider(t, ts.URL)

			temp := 0.0
			if _, err := streamAll(context.Background(), prov, llm.CompletionRequest{
				System:      []string{"judge"},
				Messages:    []llm.Message{llm.UserText("{}")},
				MaxTokens:   1024,
				Temperature: &temp,
				Thinking:    "disabled",
			}); err != nil {
				t.Fatalf("stream: %v", err)
			}
			if srv.calls() != 1 {
				t.Fatalf("upstream calls = %d, want 1", srv.calls())
			}
			got, ok := srv.bodies[0]["temperature"]
			if ok != tc.wantTemperature {
				t.Fatalf("temperature present = %v, want %v (body %v)", ok, tc.wantTemperature, srv.bodies[0])
			}
			if tc.wantTemperature && got != 0.0 {
				t.Fatalf("temperature = %v, want 0", got)
			}
		})
	}
}
