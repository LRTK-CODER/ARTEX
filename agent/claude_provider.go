package agent

import (
	"context"
	"iter"

	"github.com/Autumn-27/norma/llm"
)

// ClaudeRequestError 는 구독 호출 실패를 상위 에이전트가 자동으로 재실행하지 못하게 표시한다.
// transport의 401 갱신 한 번은 이미 끝났다. 원래 오류는 분류·수동 복구용으로 보존한다.
type ClaudeRequestError struct{ cause error }

func (e *ClaudeRequestError) Error() string { return e.cause.Error() }
func (e *ClaudeRequestError) Unwrap() error { return e.cause }

// NoReplay 는 제공자 종류를 모르는 실패 전환 풀에서도 같은 요청을 다시 보내지 않게 한다.
func (e *ClaudeRequestError) NoReplay() bool { return true }

type claudeProvider struct{ llm.Provider }

func (p claudeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	msg, stop, usage, err := p.Provider.Complete(ctx, req)
	if err != nil {
		err = &ClaudeRequestError{cause: err}
	}
	return msg, stop, usage, err
}

func (p claudeProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for event, err := range p.Provider.Stream(ctx, req) {
			if err != nil {
				yield(event, &ClaudeRequestError{cause: err})
				return
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}
