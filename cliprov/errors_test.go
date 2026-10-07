package cliprov

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/norma/llm"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want int
	}{
		{"Claude 인증 오류 코드", `{"type":"result","is_error":true,"error":"authentication_failed"}`, StatusUnauthorized},
		{"Claude OAuth 만료", "OAuth session expired and could not be refreshed", StatusUnauthorized},
		{"Codex 인증 오류 코드", `{"codexErrorInfo":"unauthorized"}`, StatusUnauthorized},
		{"로그인 안 됨", "Not logged in · Please run /login", StatusUnauthorized},
		{"Claude 세션 한도", "You've hit your session limit · resets 3pm", StatusQuotaExhausted},
		{"Codex 사용량 한도(U+2019)", "You’ve hit your usage limit. Upgrade to Pro or try again later.", StatusQuotaExhausted},
		{"Codex 한도 코드", `{"codexErrorInfo":"usageLimitExceeded"}`, StatusQuotaExhausted},
		{"Claude 크레딧 필요", `{"type":"rate_limit_event","status":"rejected","errorCode":"credits_required"}`, StatusQuotaExhausted},
		{"Codex 레이트 리밋 코드", `{"codexErrorInfo":"rateLimitExceeded"}`, StatusRateLimited},
		{"API 레이트 리밋", `API Error: 429 {"type":"rate_limit_error"}`, StatusRateLimited},
		{"알 수 없는 오류", "spawn failed: broken pipe", 0},
		{"정상 rate_limit_event 는 오류 표지가 아님", `{"type":"rate_limit_event","status":"allowed"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.msg); got != tc.want {
				t.Errorf("Classify(%q) = %d, 기대 %d", tc.msg, got, tc.want)
			}
		})
	}
}

func TestErrorString(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"You've hit your session limit", "claude-code: status 402: You've hit your session limit"},
		{"spawn failed", "claude-code: spawn failed"},
	}
	for _, tc := range cases {
		t.Run(tc.msg, func(t *testing.T) {
			if got := NewError("claude-code", tc.msg).Error(); got != tc.want {
				t.Errorf("Error() = %q, 기대 %q", got, tc.want)
			}
		})
	}
}

// scriptedProvider 는 첫 이벤트 전에 err 로 끝나거나(err != nil), 텍스트 하나를 내는 가짜다.
type scriptedProvider struct {
	err  error
	text string
}

func (p *scriptedProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if p.err != nil {
			yield(llm.StreamEvent{}, p.err)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: p.text}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func (p *scriptedProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if p.err != nil {
		return llm.Message{}, "", llm.Usage{}, p.err
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: p.text}}}, "end_turn", llm.Usage{}, nil
}

// 분류한 오류가 llmpool 에서 다음 프로필로 전환되고, 인증·한도는 즉시 차단되는지 본다.
func TestClassifiedErrorDrivesLlmpoolFailover(t *testing.T) {
	cases := []struct {
		name         string
		msg          string
		wantOpen     bool // 첫 실패로 차단기가 열리는가(hard)
		wantInLastEr string
	}{
		{"인증 만료는 즉시 차단", "OAuth session expired and could not be refreshed", true, "status 401"},
		{"한도 소진은 즉시 차단", "You’ve hit your usage limit", true, "status 402"},
		{"레이트 리밋은 전환만", "rateLimitExceeded", false, "status 429"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			health := llmpool.NewRegistry(nil, nil)
			pool := llmpool.New([]*llmpool.Member{
				{ID: 1, Name: "cli", Rank: 2, Prov: &scriptedProvider{err: NewError("claude-code", tc.msg)}},
				{ID: 2, Name: "backup", Rank: 1, Prov: &scriptedProvider{text: "from backup"}},
			}, health)

			var text strings.Builder
			for ev, err := range pool.Stream(context.Background(), llm.CompletionRequest{}) {
				if err != nil {
					t.Fatalf("전환되지 않고 오류가 났다: %v", err)
				}
				text.WriteString(ev.Text)
			}
			if text.String() != "from backup" {
				t.Errorf("응답 = %q, 기대 백업 프로필 응답", text.String())
			}
			st := health.Get(1)
			if st.Open() != tc.wantOpen {
				t.Errorf("차단기 열림 = %v, 기대 %v (state %+v)", st.Open(), tc.wantOpen, st)
			}
			if !strings.Contains(st.LastError, tc.wantInLastEr) {
				t.Errorf("LastError = %q, %q 가 없다", st.LastError, tc.wantInLastEr)
			}
		})
	}
}

func TestCheckToolName(t *testing.T) {
	tools := []llm.ToolSchema{{Name: "http_request"}, {Name: "record_finding"}}
	cases := []struct {
		name    string
		tool    string
		wantErr bool
	}{
		{"요청에 있는 도구", "http_request", false},
		{"CLI 내장 Bash", "Bash", true},
		{"CLI 내장 WebFetch", "WebFetch", true},
		{"접두사를 떼지 않은 이름", "mcp__artex__http_request", true},
		{"빈 이름", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckToolName(tc.tool, tools)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, 오류 기대 %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrUnknownTool) {
				t.Errorf("err = %v, ErrUnknownTool 이 아니다", err)
			}
		})
	}
}
