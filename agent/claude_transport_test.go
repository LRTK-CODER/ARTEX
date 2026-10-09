package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
)

func newClaudeProvider(t *testing.T, baseURL string, tokens OAuthTokenSource) llm.Provider {
	t.Helper()
	c := ConfigFrom("anthropic", "claude-sonnet-test", baseURL, "", "")
	c.AuthType = db.AuthType("claude_oauth")
	c.OAuthTokens = tokens
	// 기본값도, 사용자가 켠 재시도도 Claude 구독의 정책을 바꾸지 못해야 한다.
	c.Retry.ConnectAttempts, c.Retry.EmptyAttempts = 2, 2
	p, err := c.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeProviderIdentityAndTools(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fake-env-must-not-leak")
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer "+fakeAccessToken || r.Header.Get("x-api-key") != "" || !strings.Contains(r.Header.Get("anthropic-beta"), "oauth-2025-04-20") {
					t.Error("OAuth 요청 헤더가 틀리다")
				}
				var b map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Fatal(err)
				}
				var system []struct {
					Type string
					Text string
				}
				if err := json.Unmarshal(b["system"], &system); err != nil {
					t.Fatal(err)
				}
				if len(system) < 2 || system[0].Text != "You are Claude Code, Anthropic's official CLI for Claude." || !strings.Contains(system[1].Text, "ARTEX 지시문") {
					t.Fatalf("시스템 첫 줄·기존 지시문이 틀리다: %v", system)
				}
				if stream {
					w.Header().Set("content-type", "text/event-stream")
					_, _ = io.WriteString(w, sseEvent("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}`)+sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"Bash","input":{}}}`)+sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"id\"}"}}`)+sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)+sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`)+sseEvent("message_stop", `{"type":"message_stop"}`))
				} else {
					_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"id"}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":1}}`)
				}
			}))
			defer ts.Close()
			p := newClaudeProvider(t, ts.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})
			req := llm.CompletionRequest{System: []string{"ARTEX 지시문"}, Messages: []llm.Message{llm.UserText("도구를 호출해줘")}}
			for range 2 {
				var msg llm.Message
				var err error
				if stream {
					msg, err = streamAll(context.Background(), p, req)
				} else {
					msg, _, _, err = p.Complete(context.Background(), req)
				}
				if err != nil {
					t.Fatal(err)
				}
				uses := msg.ToolUses()
				if len(uses) != 1 || uses[0].Name != "Bash" || string(uses[0].Input) != `{"command":"id"}` {
					t.Fatalf("도구 응답이 바뀌었다: %v", uses)
				}
				req.Messages = append(req.Messages, msg, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call_1", "uid=1000", false)}})
			}
			if calls != 2 || req.System[0] != "ARTEX 지시문" {
				t.Fatal("호출 횟수·원본 지시문이 바뀌었다")
			}
		})
	}
}

func TestClaudeProviderRetriesOnlyUnauthorized(t *testing.T) {
	for _, tc := range []struct {
		name          string
		statuses      []int
		want          int
		invalidations int
	}{{"refresh once", []int{401, 200}, 2, 1}, {"repeated unauthorized", []int{401, 401, 200}, 2, 1}, {"rate limit", []int{429, 200}, 1, 0}, {"server error", []int{500, 200}, 1, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				i := int(calls.Add(1)) - 1
				status := tc.statuses[min(i, len(tc.statuses)-1)]
				w.WriteHeader(status)
				if status == 200 {
					_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`)
				} else {
					_, _ = io.WriteString(w, `{"error":{"type":"rejected","message":"rejected"}}`)
				}
			}))
			defer ts.Close()
			tokens := &fakeTokens{accessTokens: []string{fakeAccessToken, fakeRefreshedTok}}
			p := newClaudeProvider(t, ts.URL, tokens)
			_, _, _, err := p.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
			if (tc.name == "refresh once") != (err == nil) || int(calls.Load()) != tc.want || tokens.invalidations != tc.invalidations {
				t.Fatalf("모델 호출·갱신 횟수가 다르다: calls=%d invalidations=%d err=%v", calls.Load(), tokens.invalidations, err)
			}
			if err != nil {
				var noReplay *ClaudeRequestError
				if !errors.As(fmt.Errorf("worker: %w", err), &noReplay) {
					t.Fatal("상위 에이전트가 구독 오류를 다시 실행할 수 있다")
				}
			}
		})
	}
}

func TestClaudeErrorsDoNotEchoCredentials(t *testing.T) {
	for _, tc := range []struct {
		body  string
		quota bool
	}{
		{`{"error":{"type":"rate_limit_error","message":"credit balance ` + fakeAccessToken + `"}}`, false},
		{`{"error":{"type":"usage_limit_reached","message":"` + fakeAccessToken + `"}}`, true},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, tc.body)
		}))
		p := newClaudeProvider(t, ts.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})
		_, _, _, err := p.Complete(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
		ts.Close()
		if err == nil || strings.Contains(err.Error(), fakeAccessToken) || IsQuotaExhaustedMessage(err.Error()) != tc.quota {
			t.Fatal("오류에 토큰이 남거나 불명확한 429를 구독 소진으로 분류했다")
		}
	}
}

func TestClaudeMissingCredentialsDoesNotInvalidate(t *testing.T) {
	tokens := &fakeTokens{err: llmauth.ErrNoTokens}
	p := newClaudeProvider(t, "http://127.0.0.1:1", tokens)
	_, _, _, err := p.Complete(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
	if err == nil || tokens.tokenCalls != 1 || tokens.invalidations != 0 {
		t.Fatal("저장된 자격 증명이 없는데 갱신을 시도했다")
	}
}

func TestClaudeStreamErrorDoesNotExposeTokenToCapture(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\ndata: \"type\":\"error\",\ndata: \"error\":{\"type\":\"overloaded_error\",\"message\":\""+strings.ReplaceAll(fakeAccessToken, "-", `\u002d`)+"\"}\ndata: }\n\n")
	}))
	defer ts.Close()
	p := newClaudeProvider(t, ts.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})
	ctx, capture := llmrec.NewCapture(t.Context())
	_, err := streamAll(ctx, p, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
	if err == nil || strings.Contains(err.Error(), fakeAccessToken) || strings.Contains(capture.RawResponse(), fakeAccessToken) {
		t.Fatal("스트리밍 오류의 토큰이 오류·기록에 노출됐다")
	}
}

func TestClaudeRedirectAndJSONErrorDoNotExposeToken(t *testing.T) {
	for _, status := range []int{200, 302, 500} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "{\n \"type\":\"error\",\n \"error\":{\"type\":\"overloaded_error\",\"message\":\""+strings.ReplaceAll(fakeAccessToken, "-", `\u002d`)+"\"}\n}")
		}))
		p := newClaudeProvider(t, ts.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})
		ctx, capture := llmrec.NewCapture(t.Context())
		_, _, _, err := p.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
		ts.Close()
		if err == nil || strings.Contains(err.Error(), fakeAccessToken) || strings.Contains(capture.RawResponse(), fakeAccessToken) || strings.Contains(capture.RawResponse(), strings.ReplaceAll(fakeAccessToken, "-", `\u002d`)) {
			t.Fatal("JSON·리다이렉트 오류가 토큰을 반향했다")
		}
	}
}

func TestClaudePoolDoesNotResendRejectedRequest(t *testing.T) {
	for _, status := range []int{429, 500} {
		for _, stream := range []bool{false, true} {
			var headCalls, backupCalls atomic.Int32
			head := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headCalls.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"type":"rejected"}}`)
			}))
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backupCalls.Add(1); w.WriteHeader(500) }))
			pool := llmpool.New([]*llmpool.Member{
				{ID: 1, Rank: 2, Prov: newClaudeProvider(t, head.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})},
				{ID: 2, Rank: 1, Prov: newClaudeProvider(t, backup.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})},
			}, nil)
			req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}
			var err error
			if stream {
				_, err = streamAll(t.Context(), pool, req)
			} else {
				_, _, _, err = pool.Complete(t.Context(), req)
			}
			head.Close()
			backup.Close()
			if err == nil || headCalls.Load() != 1 || backupCalls.Load() != 0 {
				t.Fatal("Claude 오류를 백업 제공자에게 재전송했다")
			}
		}
	}
}

func TestClaudeResponsePreservesLargeToolInteger(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"id":9007199254740993}}],"stop_reason":"tool_use"}`)
	}))
	defer ts.Close()
	p := newClaudeProvider(t, ts.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})
	msg, _, _, err := p.Complete(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
	if err != nil || len(msg.ToolUses()) != 1 || string(msg.ToolUses()[0].Input) != `{"id":9007199254740993}` {
		t.Fatal("오류 정제가 정상 도구 입력의 큰 정수를 바꿨다")
	}
}
