package db

import (
	"encoding/json"
	"time"
)

// LLM 재시도 정책: 다섯 층 재시도의 "횟수 + 간격" 전역 설정이다.
// settings 테이블의 JSON 값 하나에 저장한다. 서버 전체에 하나뿐인 실행 파라미터라 테이블을
// 따로 만들 만하지 않다. 읽을 때 내장 기본값으로 채우므로, 키가 없으면(새 DB, 설정한 적 없음)
// 상수로 박아 두던 때와 똑같이 동작한다.

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule 은 한 층의 설정 값 한 쌍이다. 0 값은 "설정 안 됨"을 뜻한다.
//
//	Attempts   0 = 내장 기본 횟수; -1 = 이 층의 재시도 끄기; >0 = 그 값
//	IntervalMS 0 = 이 층의 원래 간격 정책(보통 지수 백오프); >0 = 고정 밀리초 간격
//
// -1은 "0회"가 아니라 "명시적으로 끄기"다. 0은 이미 "설정 안 됨"이 쓰고 있다.
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// LLMRetryPolicy 는 다섯 층의 재시도 설정을 담는다. Connect/Empty/Stream은 요청 단위 층이라
// 프로필이 덮어쓸 수 있다(LLMProfile.Retry 참고). Breaker와 Intent는 원래 프로세스 전체
// 단위라 여기에만 있다.
type LLMRetryPolicy struct {
	// Connect: SDK 연결 재시도(연결 재설정·시간 초과·429·5xx, 스트림 시작 전). 기본 3회, 지수 백오프.
	Connect RetryRule `json:"connect"`
	// Empty: SDK 빈 응답 재시도(완료됐지만 content block이 없음, openai 형식만). 기본 2회, 지수 백오프.
	Empty RetryRule `json:"empty"`
	// Stream: 같은 제공자 안의 안전 구간 재시도(출력을 넘기기 전에 끊긴 스트림을 다시 보냄). 기본 2회, 0.5초부터 지수 증가(최대 4초).
	Stream RetryRule `json:"stream"`
	// Breaker: 폴링 회로 차단기. Attempts=일시적 실패가 몇 번 이어지면 차단할지(기본 3, -1=일시적
	// 실패로는 차단하지 않음. 잔액 부족·키 무효 같은 영구 실패는 여전히 바로 차단). IntervalMS=고정
	// 대기 시간(0=기본 1/5/30분 단계).
	Breaker RetryRule `json:"breaker"`
	// Intent: 워커가 model_error로 끝난 뒤 의도 전체를 재실행. 기본 2회, 고정 3초.
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
