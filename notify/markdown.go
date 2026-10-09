package notify

import (
	"fmt"
	"strings"
)

// 이 파일은 'Markdown 계열' 알림 채널(DingTalk, WeCom)이 함께 쓰는 메시지 렌더링이다.
// Feishu는 카드 JSON, Telegram은 HTML, 이메일은 HTML을 쓰며 각자 어댑터에서 렌더링한다.

// maxAssetsShown 은 메시지에 나열할 자산의 최대 개수다. 취약점 하나에 자산이 수십 개 연결될 수 있는데,
// 모두 나열하면 메시지가 넘치고 얻는 정보도 없다. 4번째 이후의 도메인은 IM에서 아무도 보지 않는다.
const maxAssetsShown = 3

// maxSummaryRunes 는 요약을 줄일 문자 수다. IM 메시지는 '상세를 보라는 안내'이지
// 보고서 자체가 아니다. 전체 내용은 플랫폼에 있다.
const maxSummaryRunes = 120

// markdownReservedBytes 는 메시지 머리(다이제스트 줄 + 심각도 분포 + 잘림 안내)와
// 꼬리(플랫폼 링크)에 남겨 둘 양이다. 항목 단위로 묶을 때 이만큼을 예산에서 빼서 머리·꼬리가 잘리지 않게 한다.
// 머리·꼬리가 잘리면 읽는 사람이 '어느 배치인지, 표시되지 않은 항목이 몇 건인지'조차 알 수 없다.
const markdownReservedBytes = 320

// markdownEscape 는 markdown 메타 문자를 이스케이프한다.
//
// 꼭 해야 하는 이유: 취약점 제목, 요약, 유형, 자산 표시 이름은 모두 **신뢰할 수 없는 출처**에서 온다.
// 제목과 요약은 모델 출력(모델은 테스트 대상의 응답을 읽는다)이고, 자산 url은 스캔으로
// 얻은 전체 URL(대상이 조작할 수 있는 쿼리 문자열 포함)이다. 이스케이프하지 않으면 제목이
//
//	로그인 페이지 SQL 인젝션\n[긴급: 여기를 눌러 계정 확인](http://attacker.tld)
//
// 인 취약점이 보안 엔지니어의 DingTalk/Feishu에서 **클릭할 수 있는 외부 링크**로 렌더링된다. 또
// `![](http://attacker.tld/beacon)`는 렌더링할 때 클라이언트가 가져가므로
// '이 취약점을 누가 봤다'는 사실을 알리고 읽는 사람의 IP를 유출한다. 악의가 없는 내용이라도 넣어진 굵게나
// 인용 블록이 아래의 심각한 취약점을 접힘 선 밖으로 밀어낼 수 있다.
//
// 이스케이프 대상은 제목/링크/강조/목록/인용/취소선처럼 구조를 바꾸거나 클릭할 수 있는
// 요소를 만드는 문자다. `\`를 가장 먼저 처리해야 한다. 그러지 않으면 뒤에서 더한 백슬래시를 다시 이스케이프한다.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText 는 신뢰할 수 없는 텍스트를 한 줄로 만들고 이스케이프한다. markdown 본문에 쓴다.
// 한 줄로 만드는 것이 이스케이프의 나머지 절반이다. 줄바꿈만으로도 새 목록 항목이나 인용 블록을
// 꾸밀 수 있고, 이스케이프 문자로는 막지 못한다.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle 은 메시지 제목(IM 플랫폼의 제목 줄/카드 제목)을 돌려준다. 내용은 **이스케이프하지 않은 원문**이다.
//
// 여기서 일부러 이스케이프하지 않는다. 이 제목은 네 가지 문맥의 렌더러가 함께 쓴다. markdown 본문, Telegram
// HTML, Feishu 카드의 plain_text, 범용 웹훅의 JSON과 이메일 제목이다. 문맥마다
// 이스케이프 규칙이 다르다(markdown 이스케이프를 HTML에 넣으면 백슬래시가 보이고, JSON에 넣으면
// 데이터가 오염된다). 그래서 이스케이프는 각 출력 쪽이 맡는다. writeItem / feishuItemLines /
// telegramEscape 참고. 예전에 공유 함수에 markdown 이스케이프를 넣었더니 Telegram 메시지에
// `\(1\)` 같은 백슬래시가 보였다.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("취약점 다이제스트 · 총 %d건", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "취약점 알림"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody 는 메시지 본문을 렌더링하고, 본문과 **실제로 쓴 항목 수**를 돌려준다.
//
// 반환값 kept는 이번 전달로 실제 전달된 항목 수다. 호출자는 앞의 kept건만
// 전달됨으로 표시한다. 알림 채널 길이 상한에 걸려 들어가지 못한 항목은 함께 성공으로
// 표시하지 않고 다음 배치로 남겨야 한다. 이것이 바로 '소리 없는 유실'의 원인이다. 메시지는 잘렸는데
// 전달 기록은 모두 전달됐다고 나와서, 뒤쪽이 한 번도 나가지 않았다는 것을 어디서도 알 수 없다.
//
// maxBytes<=0이면 제한하지 않는다.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 단건 메시지는 너무 길어도 그대로 보낸다(마지막 자르기에 맡긴다). 취약점 하나의 일부 정보라도
		// 아예 안 보내는 것보다 낫다.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[플랫폼에서 전체 보기](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro 는 다이제스트 메시지의 첫머리(시간 범위, 건수, 심각도 분포)를 렌더링한다.
// 이것이 있으면 다이제스트를 받은 사람이 플랫폼에 들어가지 않고도 이 배치를 바로 처리해야 하는지 판단할 수 있다.
//
// items는 **실제로 들어간** 항목이고, total은 이 배치의 전체 건수다. 둘이 다르면
// '나머지 몇 건이 다음 메시지에 있는지'를 분명히 적어야 한다. 그러지 않으면 읽는 사람이 머리에 적힌 숫자를 전부로 알고,
// 한 번도 나가지 않은 뒤쪽 항목은 화면 어디에도 나타나지 않는다.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**최근 %d분 동안 새 취약점 %d건**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**새 취약점 %d건**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (이 메시지에는 앞의 %d건만 표시합니다. 나머지 %d건은 다음 메시지로 이어집니다)", len(items), extra)
	}
	// 심각도별 분포를 보여 줘 치명 항목이 있는지 한눈에 보이게 한다. **이 메시지에 실제로 든**
	// 항목만 세어, '치명 3'과 아래에서 셀 수 있는 항목 수가 맞게 한다.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem 은 취약점 항목 하나를 렌더링한다.
//
// prefix는 다이제스트 목록의 번호다. single=true면 전체판(요약과 상세 링크 포함)을 렌더링하고,
// 다이제스트 목록에서는 한 줄 요약만 렌더링한다. 그러지 않으면 50건 다이제스트가 긴 문서가 된다.
//
// 외부에서 온 내용(제목/유형/자산/요약)은 모두 markdownText를 거친다.
// 한 줄로 만들고 이스케이프한다. 상세 링크는 관리자가 설정한 public_base_url로 만든 것이라 신뢰할 수 없는 내용이 아니고,
// 클릭할 수 있는 링크여야 하므로 그대로 출력한다.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 다이제스트 모드: 한 줄로 보여 주고, 자산과 요약은 줄여서 뒤에 붙인다.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**상태 변경**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**유형**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**자산**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**요약**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[상세 보기](%s)\n", it.DetailURL)
	}
}
