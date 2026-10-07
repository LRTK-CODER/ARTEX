package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/llmauth"
	"github.com/Autumn-27/norma/llm"
)

// loginRequiredTokens 는 늘 다시 로그인해야 한다고 답하는 토큰 공급자다.
type loginRequiredTokens struct{}

func (loginRequiredTokens) Token(context.Context) (string, string, error) {
	return "", "", fmt.Errorf("load: %w", llmauth.ErrNoTokens)
}

func (loginRequiredTokens) Invalidate() {}

// TestTaskLLMDoesNotRetryChatGPTLoginRequired 는 ChatGPT 구독 로그인이 필요하다는 오류를
// 작업 체인이 같은 프로필에서 다시 시도할 일시 오류로 보지 않는지 확인한다.
func TestTaskLLMDoesNotRetryChatGPTLoginRequired(t *testing.T) {
	var upstreamCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer ts.Close()

	c := agent.ConfigFrom("openai-responses", "gpt-5-codex", ts.URL, "", "")
	c.AuthType = agent.AuthChatGPTOAuth
	c.OAuthTokens = loginRequiredTokens{}
	prov, err := c.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	var streamErr error
	for _, err := range prov.Stream(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}) {
		if err != nil {
			streamErr = err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("stream succeeded, want login required")
	}
	if isRetryableStreamError(streamErr) {
		t.Fatalf("login required error %q is treated as retryable", streamErr)
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Fatalf("upstream calls = %d, want 0", n)
	}
}
