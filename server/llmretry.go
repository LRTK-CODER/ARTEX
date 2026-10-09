package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 재시도 정책의 서버 쪽 해석이다. docs/LLM 재시도 설계.md 참고. 다섯 단계 가운데
//   - 연결 / 빈 응답 / 같은 제공자 안전 구간은 '엔드포인트를 따라가는' 단계라 LLM 프로필마다
//     전역 기본값을 덮어쓸 수 있다(프로필의 항목을 비워 두면 전역 값을 상속하고, 전역도 비어 있으면 내장 기본값을 쓴다).
//   - 회로 차단기 / 탐색 의도 재실행은 프로세스 단위라 전역 값 하나만 있다.
//
// 전역 정책은 DB의 settings 한 행을 한 번 읽는다. 호출하는 곳이 모두 드물게 도는 경로(제공자 구성, work 마무리,
// 설정 저장)라 캐시를 한 겹 더 둘 가치가 없다. 회로 차단기 파라미터는 예외다. 실패 경로에서 매번 읽어야 하므로
// applyRetryPolicy가 Registry에 넘겨 보관한다.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 횟수는 여기서 '0=기본값 / 음수=끄기'라는 원래 의미를 유지한다. SDK의 MaxRetries /
		// EmptyResponseRetries가 같은 구조이므로 SDK가 직접 해석하게 둔다.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// 회로 차단기(장애 조치 대기 시간)의 기본값은 llmpool 내장값과 같다. 여기서는 '사용자가 값을 설정했을 때'만 덮어쓴다.
// 탐색 의도 재실행의 기본값은 engine.go의 modelErrorRetries / modelErrorRetryBackoff 참고.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit 는 work 하나가 빈 턴 뒤 이어 가기 지시를 몇 번 넣을 수 있는지 정한다(steerHooks.Stop 참고).
// ② 단계의 설정값인 '빈 응답 재시도 횟수'를 일부러 함께 쓴다. 둘은 같은 문제를 다루는 두 방법이다. SDK 단계는
// '콘텐츠 블록이 하나도 없는' 경우를 맡아 같은 요청을 그대로 다시 보낸다. 여기서는 '생각만 있고 본문도 도구 호출도
// 없는' 경우를 맡아, 모델이 이미 한 생각을 이어 작업하도록 지시 하나를 덧붙인다(컨텍스트 모양 때문에 생기는 헛돌기에는
// 같은 요청을 다시 보내도 소용없다). 빈 응답을 판단하는 기준이 다른 것은 SDK가 '이벤트를 yield했는지'를 보는데
// 생각 증분도 이벤트이기 때문이다. 하지만 사용자가 '빈 응답 재시도 횟수'를 정할 때 뜻하는 것은 '모델이 실질적인
// 내용을 내지 않으면 다시 하라'이므로, 두 단계가 횟수 하나를 같이 써야 그 뜻에 맞는다.
//
// 프로필의 덮어쓰기가 아니라 전역 정책을 읽는다. run 도중 장애 조치로 프로필이 바뀔 수 있는데, 이 값은 탐색 의도
// 전체의 총량 제한이라 엔드포인트가 바뀐다고 달라지면 안 된다. 의미는 SDK의 emptyRetries()와 같다.
// 0 = 기본값 defaultEmptyTurnNudges, -1(음수) = 빈 턴 이어 가기 끄기, >0 = 그 값을 쓴다.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
