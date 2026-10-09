package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel 은 DingTalk 사용자 지정 봇을 구현한다.
//
// 플랫폼 특성(여기 구현의 선택을 정한다):
//   - 봇 하나당 분당 20건으로 속도를 제한하고, 넘치면 소리 없이 버린다(HTTP는 200일 수 있다).
//     그래서 속도 제한을 클라이언트에서 해야 한다. DefaultRatePerMin 참고.
//   - 보안 설정은 서명 / 사용자 지정 키워드 / IP 허용 목록 중 하나다. 서명만이 메시지
//     내용에 의존하지 않으므로 서명만 지원한다(셋 다 켜지 않은 맨 웹훅도 지원한다).
//   - 성공이든 실패든 HTTP 200을 돌려주고 body의 errcode로 구분한다. errcode를 검사하지 않으면
//     전달 실패가 성공으로 기록된다.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk 웹훅 주소에는 access_token이 들어 있어 주소 자체가 자격 증명이므로 전체를 마스킹한다.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 대상은 DingTalk 웹훅 주소 자체다. 주소를 바꾸면 새 주소에 대해 서명 키도 다시 입력해야 한다.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소를 입력하세요")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("잘못된 Webhook 주소입니다: %w", err)
	}
	return nil
}

// Send 는 메시지를 한 번 전달한다. 상세 링크가 있는 단건이면 ActionCard(버튼 포함)를, 아니면 markdown을 쓴다.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown 본문에는 분명한 바이트 상한이 없지만, 증거 필드가 비정상적으로 커지지 않게 상한을 둔다.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "상세 보기",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk은 업무 오류를 200 응답 안에 숨긴다.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("DingTalk 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000은 서명 검증 실패, 310000은 키워드 불일치다. 둘 다 설정 오류라
		// 재시도해도 저절로 낫지 않는다.
		return 0, Permanent(fmt.Errorf("DingTalk 오류 응답 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL 은 공식 서명 규칙대로 webhook에 timestamp와 sign 파라미터를 붙인다.
//
// 규칙: 서명할 문자열 = timestamp + "\n" + secret이고, HMAC-SHA256의 **키도 secret**이다.
// 결과를 base64한 뒤 URL 인코딩한다. timestamp는 밀리초다. secret이 비어 있으면 그대로 돌려줘
// 서명을 켜지 않은 봇을 지원한다.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err를 그대로 넘기지 않는다. url.Parse의 오류 텍스트에는 전체 주소(access_token 포함)가 들어 있다.
		return "", fmt.Errorf("Webhook 주소 파싱 실패: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL 은 주소를 쓸 수 있는지, 지원하는 프로토콜인지 검사하고, 리터럴 IP 대상은 내부망인지 판단한다.
//
// 신경 쓴 두 가지:
//
//  1. **오류 메시지는 민감 정보를 가려야 한다**. url.Parse가 돌려주는 *url.Error의 Error()에는
//     **원래 주소 전체**가 들어가는데, 이 기능의 알림 채널 주소에는 자격 증명이 박혀 있다(DingTalk access_token,
//     WeCom key, Telegram bot token, Feishu hook id). 예전에는 여기서 바로 `return err`를 해서
//     '주소 형식이 잘못됐다'는 오류가 자격 증명을 밖으로 내보냈다. 테스트 API의 400 응답,
//     전달할 때마다 저장되는 last_error, 서버 로그와 전달 이력 API로 흘러갔다.
//
//  2. **리터럴 IP는 바로 내부망인지 판단하고**, 도메인은 연결 단계에 맡긴다(blockInternalDial이 최종
//     적용 지점이고 DNS 리바인딩도 막는다). 여기서 한 번 하는 것은 설정을 저장할 때 바로 안내하기 위해서이고,
//     첫 전달이 실패할 때까지 기다리지 않게 하려는 것이다.
//
// 프로토콜 제한은 방어용이다. file:///gopher:// 같은 것은 http.Client가 예상하지 못한
// 동작을 하게 만든다(scheme 검사에서 이미 막지만, 이 면을 열어 둘 이유가 없다).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 파싱할 수 없습니다(%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http/https만 지원합니다. 받은 값: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("주소에 호스트 이름이 없습니다")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("로컬/링크 로컬 주소 %s(으)로는 전달하지 않습니다(로컬 서비스로 꼭 전달해야 하면 %s=1로 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
