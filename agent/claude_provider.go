package agent

import (
	"context"
	"errors"
	"iter"

	"github.com/Autumn-27/norma/llm"
)

// ErrModelRefused 는 제공자가 응답 종료 사유로 명시적 거절(stop_reason=refusal)을 돌려줬다는 뜻이다.
// 같은 요청을 다시 보내거나 다른 프로필로 넘겨도 결과가 달라지리라 볼 근거가 없으므로, 호출자는
// errors.Is로 이 오류를 가려 작업을 멈춘다. 응답만으로는 무엇 때문에 거절했는지 알 수 없다.
var ErrModelRefused = errors.New("모델이 요청을 거절했습니다(stop_reason=refusal)")

// claudeStopRefusal 은 Anthropic Messages API가 명시적 거절에 쓰는 종료 사유다.
const claudeStopRefusal = "refusal"

// ClaudeRequestError 는 구독 호출 실패를 상위 에이전트가 자동으로 재실행하지 못하게 표시한다.
// transport의 401 갱신 한 번은 이미 끝났다. 원래 오류는 분류·수동 복구용으로 보존한다.
type ClaudeRequestError struct{ cause error }

func (e *ClaudeRequestError) Error() string { return e.cause.Error() }
func (e *ClaudeRequestError) Unwrap() error { return e.cause }

// NoReplay 는 제공자 종류를 모르는 실패 전환 풀에서도 같은 요청을 다시 보내지 않게 한다.
func (e *ClaudeRequestError) NoReplay() bool { return true }

// ProfileHealthy 는 명시적 거절이 프로필 장애가 아니라 모델의 판단임을 실패 전환 풀에 알린다.
// 풀은 이때 회로 차단기에 실패를 기록하지 않아, 거절이 이어져도 다음 요청이 백업 프로필로 가지 않는다.
func (e *ClaudeRequestError) ProfileHealthy() bool { return errors.Is(e.cause, ErrModelRefused) }

type claudeProvider struct{ llm.Provider }

func (p claudeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	msg, stop, usage, err := p.Provider.Complete(ctx, req)
	if err == nil && stop == claudeStopRefusal {
		err = ErrModelRefused
	}
	if err != nil {
		// 거절도 usage를 그대로 돌려줘 llmrec이 입력 토큰 사용량을 기록하게 한다.
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
			// 거절을 알리는 message_delta는 usage를 싣고 있으므로 먼저 넘겨 기록되게 한 뒤 오류로 끝낸다.
			// 그대로 두면 빈 메시지가 정상 종료로 받아들여져 작업이 아무것도 하지 않고 시간 초과까지 남는다.
			if event.Type == llm.SEMessageDelta && event.StopReason == claudeStopRefusal {
				yield(llm.StreamEvent{}, &ClaudeRequestError{cause: ErrModelRefused})
				return
			}
		}
	}
}
