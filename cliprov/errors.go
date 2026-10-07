package cliprov

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// 아래 상태 코드는 llmpool 이 오류 문자열의 "status NNN" 으로 실패 전환과 차단을
// 정하는 규약에 맞춘 것이다(llmpool/pool.go 의 statusOf). CLI 오류에는 HTTP 상태가
// 없으므로 메시지로 분류해 같은 규칙을 타게 한다.
const (
	// StatusUnauthorized 는 로그인 만료·인증 실패다. llmpool 이 즉시 차단한다.
	StatusUnauthorized = 401
	// StatusQuotaExhausted 는 구독 사용량 한도 소진이다. 재설정 시각까지 풀리지 않으므로
	// llmpool 이 즉시 차단하게 402 로 올린다.
	StatusQuotaExhausted = 402
	// StatusRateLimited 는 일시 레이트 리밋이다. llmpool 이 전환하되 바로 차단하지는 않는다.
	StatusRateLimited = 429
)

// statusMarkers 는 상태별 표지어다. 앞 항목이 먼저 맞는다. "usage limit" 이 "rate limit"
// 보다 앞에 와야 한도 소진이 레이트 리밋으로 잘못 분류되지 않는다.
// 표본 출처: #6 조사 보고서(Claude·Codex 오류 문구와 오류 코드).
var statusMarkers = []struct {
	status  int
	markers []string
}{
	{StatusUnauthorized, []string{
		"authentication_failed",
		"unauthorized",
		"oauth session expired",
		"not logged in",
	}},
	{StatusQuotaExhausted, []string{
		"hit your session limit",
		"hit your usage limit",
		"hit your weekly limit",
		"usagelimitexceeded",
		"credits_required",
	}},
	{StatusRateLimited, []string{
		"ratelimitexceeded",
		"rate_limit_error",
		"rate limit",
		"too many requests",
	}},
}

// Classify 는 CLI 오류 메시지를 llmpool 규약의 상태 코드로 분류한다.
// 알 수 없는 메시지는 0 이다(llmpool 은 상태 없는 오류를 전송 실패로 보고 전환한다).
func Classify(msg string) int {
	// Codex 는 "You’ve" 처럼 U+2019 따옴표를 쓴다. 대소문자와 함께 맞춘다.
	normalized := strings.ToLower(strings.ReplaceAll(msg, "’", "'"))
	for _, group := range statusMarkers {
		for _, m := range group.markers {
			if strings.Contains(normalized, m) {
				return group.status
			}
		}
	}
	return 0
}

// Error 는 provider 가 올리는 CLI 오류다. Error() 문자열이 llmpool 규약
// "<prefix>: status <NNN>: <원문>" 을 따른다.
type Error struct {
	// Prefix 는 provider 이름이다(예: "claude-code").
	Prefix string
	// Status 는 Classify 결과다. 0 이면 문자열에 status 를 넣지 않는다.
	Status int
	// Message 는 CLI 가 낸 오류 원문이다.
	Message string
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s: %s", e.Prefix, e.Message)
	}
	return fmt.Sprintf("%s: status %d: %s", e.Prefix, e.Status, e.Message)
}

// NewError 는 msg 를 분류해 Error 를 만든다.
func NewError(prefix, msg string) *Error {
	return &Error{Prefix: prefix, Status: Classify(msg), Message: msg}
}

// ErrUnknownTool 은 CLI 가 요청에 없는 도구를 부르려 했을 때 올린다. 대상 데이터의
// 주입 문장이 CLI 내장 도구(Bash 등)를 부르게 했을 수 있으므로 실행하지 않는다.
var ErrUnknownTool = errors.New("cliprov: 요청에 없는 도구 호출")

// CheckToolName 은 CLI 가 돌려준 도구 이름이 tools 에 있는지 본다. provider 는
// MCP 접두사(mcp__<서버>__)를 뗀 이름을 넘긴다. 없으면 ErrUnknownTool 을 감싸 올린다.
func CheckToolName(name string, tools []llm.ToolSchema) error {
	for _, t := range tools {
		if t.Name == name {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnknownTool, name)
}
