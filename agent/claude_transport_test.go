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

// claudeRefusalServer 는 HTTP 200으로 명시적 거절(stop_reason=refusal)을 돌려주는 가짜 Claude 서버다.
// 시작 usage는 입력 10토큰, 종료 usage는 출력 0토큰으로 실제 진단에서 본 모양을 따른다.
func claudeRefusalServer(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseEvent("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`)+
				sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":0}}`)+
				sseEvent("message_stop", `{"type":"message_stop"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","content":[],"stop_reason":"refusal","usage":{"input_tokens":10,"output_tokens":0}}`)
	}))
}

func TestClaudeRefusalIsNonReplayableError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{{"complete", false}, {"stream", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ts := claudeRefusalServer(&calls)
			defer ts.Close()
			p := newClaudeProvider(t, ts.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})
			req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("목표를 분해해줘")}}
			var usage llm.Usage
			var err error
			if tc.stream {
				acc := llm.NewAccumulator()
				for ev, streamErr := range p.Stream(t.Context(), req) {
					if streamErr != nil {
						err = streamErr
						break
					}
					acc.Add(ev)
				}
				usage = acc.Usage
			} else {
				_, _, usage, err = p.Complete(t.Context(), req)
			}
			if err == nil {
				t.Fatal("명시적 거절이 오류 없이 빈 성공 응답으로 돌아왔다")
			}
			if !strings.Contains(err.Error(), "refusal") {
				t.Fatalf("오류에 거절 사유가 없다: %v", err)
			}
			var noReplay *ClaudeRequestError
			if !errors.As(fmt.Errorf("planner: %w", err), &noReplay) {
				t.Fatal("상위 계층이 거절된 요청을 다시 보낼 수 있다")
			}
			if usage.InputTokens != 10 {
				t.Fatalf("거절 응답의 토큰 사용량을 잃었다: %+v", usage)
			}
			if calls.Load() != 1 {
				t.Fatalf("거절된 요청을 다시 보냈다: calls=%d", calls.Load())
			}
		})
	}
}

func TestClaudePoolDoesNotResendRefusedRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var headCalls, backupCalls atomic.Int32
		head := claudeRefusalServer(&headCalls)
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
			t.Fatalf("거절된 요청을 다시 보내거나 백업 제공자에게 넘겼다: stream=%v err=%v head=%d backup=%d", stream, err, headCalls.Load(), backupCalls.Load())
		}
	}
}

// 거절은 모델의 판단이지 프로필 장애가 아니다. 거절이 회로 차단 기준(softTripAfter)보다 많이 이어져도
// 회로가 열리지 않고, 그다음 요청도 백업 프로필로 넘어가지 않아야 한다.
func TestClaudeRefusalDoesNotTripPoolBreaker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{{"complete", false}, {"stream", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var headCalls, backupCalls atomic.Int32
			head := claudeRefusalServer(&headCalls)
			t.Cleanup(head.Close)
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backupCalls.Add(1); w.WriteHeader(500) }))
			t.Cleanup(backup.Close)
			reg := llmpool.NewRegistry(nil, nil)
			pool := llmpool.New([]*llmpool.Member{
				{ID: 1, Rank: 2, Prov: newClaudeProvider(t, head.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})},
				{ID: 2, Rank: 1, Prov: newClaudeProvider(t, backup.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})},
			}, reg)
			req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}
			for i := 1; i <= 4; i++ {
				var err error
				if tc.stream {
					_, err = streamAll(t.Context(), pool, req)
				} else {
					_, _, _, err = pool.Complete(t.Context(), req)
				}
				if !errors.Is(err, ErrModelRefused) {
					t.Fatalf("%d번째 호출이 거절 오류로 끝나지 않았다: %v", i, err)
				}
			}
			if headCalls.Load() != 4 || backupCalls.Load() != 0 || reg.IsOpen(1) || reg.Get(1).Fails != 0 {
				t.Fatalf("거절을 프로필 장애로 셌다: head=%d backup=%d open=%v fails=%d",
					headCalls.Load(), backupCalls.Load(), reg.IsOpen(1), reg.Get(1).Fails)
			}
		})
	}
}
