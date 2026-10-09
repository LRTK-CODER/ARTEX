package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel 은 알림 채널 하나의 어댑터다. 구현은 **상태가 없어야** 한다. 같은 인스턴스를 여러 알림 채널
// 설정이 동시에 함께 쓰므로 자격 증명은 모두 cfg 파라미터로 받는다.
type Channel interface {
	// Kind 는 알림 채널 유형 식별자를 돌려준다. 레지스트리의 키와 같아야 한다.
	Kind() string
	// Validate 는 설정을 저장할 때 불리며 필수 필드와 형식을 검사한다. 돌려준 오류는 설정하는 사람에게
	// 그대로 보이므로, 막연한 '설정이 잘못됐습니다'가 아니라 '어느 필드가 빠졌는지'를 말해야 한다.
	Validate(cfg map[string]any) error
	// Send 는 메시지를 한 번 전달하고 **실제로 전달된 항목 수**와 오류를 돌려준다.
	//
	// 건수를 돌려주는 이유: 플랫폼마다 메시지 길이 상한이 있어서, 다이제스트 메시지에 배치 전체가 들어가지 않으면 잘린다.
	// 호출자가 배치 전체를 무조건 전달됨으로 표시하면 잘린 항목은 사라진다. 메시지에도 보이지 않고,
	// 전달 이력에도 성공으로 나와 취약점이 한 번도 나가지 않았다는 것을 어디서도 알 수 없다. kept를 돌려주면
	// 호출자는 앞의 kept건만 표시하고 나머지는 다음 배치로 남긴다.
	//
	// 오류를 돌려주면 전달하지 못한 것이고, 그중 *PermanentError는 재시도하면 안 된다는 뜻이다.
	// 실패했을 때 kept는 의미가 없으므로 호출자는 무시해야 한다.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin 은 이 알림 채널의 공식 권장 분당 전달 상한을 돌려준다. 알림 채널 인스턴스를 새로 만들 때
	// 기본 발송 속도 제한값으로 쓴다. 0을 돌려주면 알려진 제한이 없는 것이다.
	DefaultRatePerMin() int
	// SecretKeys 는 이 알림 채널 설정에서 자격 증명에 해당하는 키 이름을 돌려준다. API가 값을 돌려줄 때 이 키의 값은 마스킹되고,
	// 갱신할 때 마스킹된 값을 받으면 DB의 원래 값을 유지한다. 어느 필드가 자격 증명인지는 구현만 정확히 안다
	// (WeCom은 웹훅 주소 전체가 자격 증명이지만 DingTalk은 그중 secret만이다).
	// 그래서 이 지식은 위 계층이 추측하지 않고 알림 채널이 제공해야 한다.
	SecretKeys() []string
	// DestinationKeys 는 이 알림 채널 설정에서 '메시지를 어디로 보낼지' 정하는 키 이름을 돌려준다.
	//
	// SecretKeys 처럼 보안과 관련된 것이다. 대상 주소와 자격 증명은 서로 독립된 필드다.
	// '주소만 바꾸고 자격 증명은 그대로 유지'를 허용하면 알림 채널 설정을 바꿀 수 있는 누구나 DB의 진짜 자격 증명을
	// 자기가 통제하는 서버로 보낼 수 있어, 알림 채널 설정의 마스킹이 아무 의미가 없어진다.
	// 자세한 것은 PrepareConfigUpdate 참고.
	DestinationKeys() []string
}

// registry 는 알림 채널 레지스트리다. init() 자체 등록 대신 명시적 리터럴을 일부러 쓴다. 그래야 '어떤 알림 채널이 있는지'를
// 한곳에서 다 볼 수 있고, 알림 채널을 더할 때 빠뜨린 것이 런타임 부작용이 아니라 컴파일할 때 드러난다.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get 은 유형으로 알림 채널 구현을 찾는다.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind 는 kind가 지원하는 알림 채널 유형인지 알려 준다.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds 는 지원하는 알림 채널 유형 전체를 사전순으로 돌려준다(UI 드롭다운이 안정된 순서로 보이게).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError 는 재시도하면 안 되는 전달 실패를 표시한다. 자격 증명 오류, 대상의 거부, 잘못된 요청 본문 등이다.
// 재시도는 일시적인 장애(네트워크 불안정, 속도 제한, 상대 5xx)에만 의미가 있다. 영구 실패를 백오프하며 계속 재시도하면
// 성공하지도 못하고, 진짜 오류가 재시도 로그에 묻혀 버린다.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent 는 err를 영구 실패로 표시한다. err가 nil이면 nil을 돌려주므로
// `return Permanent(someCheck())`처럼 쓸 수 있다.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent 는 err 체인에 영구 실패 표시가 있는지 알려 준다.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 설정 읽기 helper ----
//
// 알림 채널 설정은 DB의 JSONB 열에서 오고, encoding/json으로 역직렬화하면 map[string]any가 된다.
// 숫자는 모두 float64, 배열은 []any다. 아래 helper가 이 변환을 한곳에서 맡고, 사용자가
// UI에서 비워 두어 생기는 타입 차이(포트를 문자열로 넣는 등)를 너그럽게 받아 준다.

// cfgString 은 문자열 설정 항목을 읽고 앞뒤 공백을 모두 잘라 낸다. 웹 폼에서 복사해 붙이면 쉽게 따라붙는다.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt 는 정수 설정 항목을 읽는다. float64(JSON 기본)와 문자열 두 출처를 모두 받는다.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool 은 불리언 설정 항목을 읽는다. 문자열 "true"/"1"도 받는다.
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings 는 문자열 배열 설정 항목을 읽고, 공백을 자르고 빈 문자열은 버린다.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 값이 하나일 때 폼 제출이 쉽도록 문자열 하나도 받는다.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap 은 문자열 맵 설정 항목(사용자 지정 HTTP 헤더 등)을 읽는다. 키와 값의 공백을 자르고 빈 키는 버린다.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
