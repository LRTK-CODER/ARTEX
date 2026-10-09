package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes 는 s를 max 바이트 이하로 자른다. 결과가 올바른 UTF-8이고 문자를 끊지 않음을 보장한다.
//
// 문자 경계에서 잘라야 하는 이유: WeCom 그룹 봇의 markdown은 4096 **바이트** 고정 상한이 있는데(문자
// 수가 아니다), 한글·한자 한 글자는 3바이트다. 바이트 단위로 바로 자르면 한 글자가 반으로 갈려 잘못된
// UTF-8이 된다. 플랫폼은 메시지 전체를 거부하거나 깨진 문자로 보여 준다. 그래서 예산 위치에서
// 가장 가까운 rune 시작 바이트까지 뒤로 물러난다(utf8.RuneStart가 연속 바이트 0b10xxxxxx를 가린다).
//
// max<=0이면 제한하지 않는다. 자른 뒤에는 말줄임표를 붙인다. 단, max가 말줄임표도 담지 못할 만큼 작으면 붙이지 않는다.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max가 말줄임표보다 짧다. 말줄임표를 버리고 그냥 자른다. 결과가 max를 넘지 않게 하려는 것이다.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine 은 여러 줄 텍스트를 한 줄로 만든다. 공백을 모두 접은 뒤 문자 수로 자른다.
// IM 메시지의 제목 줄에 쓴다. 요약에는 줄바꿈이 흔해서 표·제목에 그대로 넣으면 배치가 깨진다.
// max<=0이면 길이를 제한하지 않는다.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes 는 s를 max 문자(바이트가 아니다) 이하로 자르고, 넘으면 말줄임표를 붙인다.
// max<=0이면 제한하지 않는다.
//
// TruncateBytes 와 다른 점은 플랫폼의 길이 기준이다. WeCom은 바이트로, Telegram은 문자 수로 길이를 제한한다.
// 기준을 잘못 쓰면 오류는 나지 않고 메시지만 예상보다 훨씬 짧게 잘린다(한글·한자 1자 = 3바이트라
// 4096바이트로 자르면 약 1365자만 남는다). 그래서 두 함수를 모두 두고 알림 채널마다 골라 쓴다.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML 은 HTML 조각을 문자 수로 자르되 반쪽 태그가 생기지 않게 한다.
//
// HTML을 문자 단위로 바로 자르면 `<a href="htt` 같은 깨진 태그가 생긴다. 플랫폼 파서는 메시지 전체를
// 오류로 거부하거나 뒤의 본문을 속성값으로 삼켜 버린다. 그래서 먼저 문자 수로 자른 뒤,
// 끝에 닫히지 않은 `<`가 있으면 그 앞으로 물러난다.
//
// 태그 짝 맞추기(</b> 보충 등)는 하지 않는다. Telegram HTML 파서가 닫히지 않은 태그를 알아서 닫고,
// 직접 짝을 맞추려면 속성 안의 따옴표·주석·자체 닫힘 태그를 다뤄야 해서 복잡도에 비해 얻는 것이 적다.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 끝이 `<`로 시작하는 조각이면(마지막 `<` 뒤에 `>`가 없으면) `<` 앞으로 물러난다.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 끝이 잘린 HTML 엔티티(예: `&amp;`가 `&amp`로 잘림)여도 물러난다.
	// 엔티티 조각은 엔티티만 받는 파서에서 **메시지 전체**를 거부당하게 할 수 있다. 길이
	// 상한을 넘는 다이제스트 메시지는 흔하므로, 이것 때문에 알림 전체를 잃을 이유가 없다.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount 는 예산 안에 **온전히** 들어가는 항목 수를 계산한다. 다이제스트 메시지를 항목 단위로 묶을 때 쓴다.
//
// 전체를 렌더링한 뒤 자르지 않고 항목 단위로 묶는 이유: 자르면 뒤쪽 항목이 소리 없이 사라지는데,
// 그 전달 기록은 여전히 전달됨으로 표시된다. 메시지에서도 전달 이력에서도 알 수 없어
// 취약점이 그대로 사라진다. 항목 단위로 묶으면 들어가지 못한 항목은 DB에 남아 다음 배치가 되고,
// 호출자가 받는 kept가 이 메시지로 실제 전달된 건수가 된다.
//
// 파라미터: maxSize<=0이면 제한하지 않는다. reserve는 메시지 머리·꼬리에 남겨 둘 양이다.
// size는 길이를 잰다(플랫폼마다 기준이 다르다. WeCom/DingTalk은 바이트, Telegram은 문자 수.
// 기준을 잘못 쓰면 오류는 나지 않고 한글·한자 메시지가 상한보다 훨씬 작게 줄어든다).
// render는 idx번째 항목을 실제 텍스트로 렌더링한다. 길이는 내용에 따라 달라 추정할 수 없다.
//
// 항목이 남아 있으면 최소 1을 돌려준다. 한 항목이 극단적으로 길어도 그 항목은 보내고 호출자의
// 마지막 자르기에 맡긴다. 그러지 않으면 너무 긴 취약점 하나가 배치 전체를 영원히 막는다.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize 는 packItemCount 의 두 가지 길이 기준이다. 이름을 붙여 두어 호출하는 곳에
// 이름 없는 func(s string) int 클로저가 나오지 않게 한다. 그러면 어느 기준을 쓰는지 한눈에 알기 어렵다.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine 은 자산 목록을 표시용 한 줄로 렌더링한다. limit개를 넘으면 나머지를 생략하고 전체 수를 적는다.
// 취약점 하나에 자산이 수십 개 연결될 수 있어 모두 나열하면 메시지가 넘친다.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " 외 총 " + itoa(len(assets)) + "개"
}

// itoa 는 strconv.Itoa 의 짧은 별칭이다. 표시 텍스트를 이어 붙일 때만 쓰며, 곳곳에서 strconv를 import하지 않으려는 것이다.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
