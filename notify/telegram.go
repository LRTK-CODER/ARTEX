package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit 은 Telegram sendMessage text 필드의 상한(문자 수)이다.
const telegramTextLimit = 4096

// telegramChannel 은 Telegram Bot API를 구현한다.
//
// 플랫폼 특성:
//   - 인증은 모두 URL path 안에 있다(/bot<token>/sendMessage). 서명이 필요 없다.
//   - MarkdownV2 대신 HTML 파싱 모드를 쓴다. MarkdownV2는 `_*[]()~`>#+-=|{}.!`
//     18개 문자를 이스케이프해야 하고 하나만 빠져도 메시지 전체가 거부된다. HTML은 & < > 세 개만 이스케이프하면 된다.
//   - 업무 오류도 HTTP 200 안에 숨기므로 ok 필드로 판단한다.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram은 1:1 대화에서 초당 약 1건, 그룹에서 분당 20건이다. 보수적인 값을 쓴다.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token은 완전한 자격 증명이다. chat_id는 받는 사람일 뿐 비밀이 아니다(그것만으로는 Token 없이 메시지를 보낼 수 없다).
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url은 Token을 어느 API 엔드포인트(자체 구축 리버스 프록시 등)로 보낼지 정하므로, 바꾸면 Token을 다시 입력해야 한다.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token을 입력하세요")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID를 입력하세요")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("잘못된 API 주소입니다: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429는 속도 제한이라 백오프 뒤 재시도가 통한다. 나머지(400 파라미터 오류, 401 token 오류, 403 차단됨,
	// 404 chat 없음)는 모두 설정 문제라 재시도해도 저절로 낫지 않는다.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram 속도 제한: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram 오류 응답 %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint 는 sendMessage 주소를 만든다. base_url이 비어 있으면 공식 API를 쓰고,
// 값이 있으면 자체 구축 Bot API 리버스 프록시에 쓴다(중국 내 네트워크에서 흔히 필요하다).
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err를 그대로 넘기지 않는다. 주소에 Bot Token이 들어 있고, 이때는 addr조차 돌려 보여 주면 안 된다.
		return "", fmt.Errorf("API 주소 조합 실패(API 주소: %s)", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML 은 HTML 본문을 렌더링하고, 본문과 실제로 쓴 항목 수를 돌려준다(Channel.Send 참고).
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram의 상한은 **문자 수**라서 묶을 때도 문자 수로 잰다(runeSize).
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">플랫폼에서 전체 보기</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>상태 변경</b>: %s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>유형</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>자산</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>요약</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">상세 보기</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes 는 메시지 제목과 잘림 안내에 남겨 둘 양이다(문자 수 기준).
const telegramReservedRunes = 160

// telegramBatchLine 은 다이제스트의 항목 하나를 렌더링한다(이스케이프하지 않으며, 호출자가 한꺼번에 이스케이프한다).
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle 은 다이제스트 메시지의 제목 줄을 렌더링한다. 건수는 이 배치 전체가 아니라 **이 메시지에 실제로 든**
// 건수를 쓴다. 그러지 않으면 읽는 사람이 머리에 적힌 숫자를 전부로 안다.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("취약점 다이제스트 · 총 %d건", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf(" (앞의 %d건만 표시합니다. 나머지 %d건은 다음 메시지로 이어집니다)", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("최근 %d분 · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape 는 HTML 텍스트 내용을 이스케이프한다.
// Telegram은 이 세 엔티티만 알아본다. 이스케이프하면 &amp; 같은 기존 엔티티도 한 번 더 이스케이프되는데, 이것이
// 바라는 동작이다. 보여 주려는 것은 원래 문자이지 사용자가 HTML을 넣게 하려는 것이 아니다.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr 는 HTML 속성값을 이스케이프한다. 텍스트 이스케이프에 더해 따옴표도 처리해야 한다.
// URL에 따옴표가 있으면 href 속성이 일찍 닫혀 뒤의 내용이 인젝션 지점이 된다.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
