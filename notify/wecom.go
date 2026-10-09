package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit 은 WeCom 그룹 봇 markdown content의 고정 상한이다(바이트, 문자가 아니다).
// 여섯 알림 채널 중 가장 빡빡한 제한이며 TruncateBytes가 있는 주된 이유다.
const weComMarkdownLimit = 4096

// weComChannel 은 WeCom 그룹 봇을 구현한다.
//
// 플랫폼 특성:
//   - URL의 key로만 인증하고 서명을 지원하지 않는다. 그래서 webhook 주소 자체가 자격 증명 전부다.
//   - markdown content 상한은 4096 **바이트**이고, 넘으면 잘리지 않고 메시지 전체가 거부된다. 한글·한자는 글자당 3바이트라
//     본문에 천여 자만 쓸 수 있어 클라이언트에서 잘라야 한다.
//   - 분당 20건으로 속도를 제한하며, 역시 클라이언트 속도 제한으로 막는다.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom은 자격 증명이 웹훅 한 곳(URL의 key)뿐이고 서명도 지원하지 않는다.
// 주소 전체가 자격 증명이라 마스킹할 다른 필드가 없다.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom은 웹훅 필드 하나가 목적지이자 자격 증명이라 '주소를 바꾼 뒤 남는 자격 증명'이 생기지 않는다.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소를 입력하세요")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("잘못된 Webhook 주소입니다: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 다이제스트 배치는 길 수 있어서(50건 × 건당 한 줄 + 머리말) 4096바이트를 쉽게 넘는다.
	// 플랫폼 오류에 맡기지 않고 여기서 자른다. 거부되면 배치 전체를 잃지만, 자르면 앞의 몇 건은 전달된다.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("WeCom 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009는 API 호출 한도 초과다. 플랫폼의 속도 제한 창은 굴러가므로 백오프 뒤 재시도가 통한다.
		// 그래서 명시적으로 재시도 가능으로 둔다. 여기까지 왔다면 클라이언트 rate_per_min이 너무 공격적으로 설정된 것이고,
		// 재시도는 대비책일 뿐이다. 제대로 고치려면 이 알림 채널의 발송 속도 제한값을 낮춘다.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom 속도 제한 %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000은 webhook key가 잘못된 것이다. 영구 실패라 재시도해도 저절로 낫지 않는다.
		return 0, Permanent(fmt.Errorf("WeCom 오류 응답 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
