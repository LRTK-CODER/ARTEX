package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// 이 패키지에서 가장 중요한 불변식이다. WeCom은 **바이트**로 길이를 제한하고 한글·한자는 글자당 3바이트라,
	// 바이트로 그냥 자르는 구현은 모두 글자를 반으로 갈라 잘못된 UTF-8을 만들고 플랫폼에 거부된다.
	// 길이가 서로소인 여러 한영 혼합 입력으로 가능한 모든 자르는 지점을 두드린다.
	inputs := []string{
		"한글테스트내용",
		"혼합 mixed 내용 content",
		"a한b글c테d스e",
		"🔴🟠🟡🔵", // 4바이트 이모지라 잘못 자르면 더 잘 드러난다
		strings.Repeat("취약점", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("입력 %q max=%d: 잘못된 UTF-8 %q를 만듦", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("입력 %q max=%d: 결과 %d바이트가 상한을 넘음", in, max, len(got))
			}
			// 잘리지 않았으면 내용을 바꾸면 안 된다.
			if len(in) <= max && got != in {
				t.Fatalf("입력 %q max=%d: 상한을 넘지 않았는데 내용을 바꿈 -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0은 제한 없음이어야 함")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0은 제한 없음이어야 함")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max가 말줄임표 자체보다 작을 때, 말줄임표를 붙이느라 오히려 상한을 넘으면 안 된다.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1일 때 결과 %q 길이 %d가 상한을 넘음", got, len(got))
	}
	// 보통은 말줄임표가 붙어야 한다.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("말줄임표가 있어야 함, 실제값 %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes 와의 기준 차이는 유지해야 한다. Telegram은 문자 수로 길이를 제한하는데,
	// 바이트 기준을 쓰면 한글 메시지가 3분의 1만 남는다.
	s := "가나다라마바사아자차"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("문자 수 = %d, 기대값 5 (%q)", n, got)
	}
	// 같은 문자열을 바이트 기준으로 자르면 분명히 더 짧아야 한다.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("바이트 기준이 문자 기준과 같은 문자 수를 만들면 안 됨")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("첫째 줄\n\n둘째 줄\t탭 포함   여러 공백", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("모든 공백을 접어야 함, 실제값 %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("연속 공백을 남기면 안 됨, 실제값 %q", got)
	}
	// 자른 뒤에도 읽을 수 있고 올발라야 한다.
	got = OneLine("가나다라마바사아자차", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("문자 수 = %d, 기대값 4 (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML을 그냥 자르면 `<a href="htt` 같은 조각이 생기고 플랫폼이 메시지 전체를 거부한다.
	s := `<b>제목</b>본문본문본문<a href="https://example.com/very/long/path">상세 보기</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 결과 %d자가 상한을 넘음", max, n)
		}
		// 끝에 닫히지 않은 `<`가 있으면 안 된다(마지막 부분에 `<`가 있는데 `>`가 없는 경우).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 끝의 태그가 잘림 -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("자산이 없으면 빈 문자열이어야 함, 실제값 %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("상한을 넘지 않으면 모두 나열해야 함, 실제값 %q", got)
	}
	// 상한을 넘으면 전체 수를 적어야 한다. 그러지 않으면 읽는 사람이 나열되지 않은 자산이 몇 개인지 모른다.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "외 총 5개") {
		t.Fatalf("전체 수 5를 적어야 함, 실제값 %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("빈 심각도의 순번은 0이라 어떤 기준에도 걸러져야 함")
	}
	if !AtLeast("critical", "") {
		t.Fatal("빈 기준은 통과시켜야 함")
	}
	if got := StatusLabel("fixed"); got != "수정됨" {
		t.Fatalf("상태 대응 오류, 실제값 %q", got)
	}
	// 모르는 상태는 레이블을 지어내지 않고 그대로 돌려준다.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("모르는 상태는 그대로 돌려줘야 함, 실제값 %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity 는 감사에서 지적된 누락 하나를 다룬다. 자를 때는 반쪽
// 태그뿐 아니라 잘린 HTML 엔티티도 피해야 한다.
//
// `&amp;`가 `&amp`로 잘리면 엔티티만 받는 파서가 메시지 **전체**를 거부할 수 있다.
// 너무 긴 다이제스트 메시지는 원래 흔하므로 대가가 너무 크다.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 끝에 '&는 있는데 짝이 되는 ;가 없는' 엔티티 조각이 나오면 안 된다.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 끝에 엔티티 조각이 남음 %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 잘못된 엔티티가 나옴", max)
		}
	}
}
