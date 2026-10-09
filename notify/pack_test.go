package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 이 파일은 '항목 단위로 묶기' 수정을 다룬다. 다이제스트 메시지가 알림 채널 길이 상한을 넘으면 **항목 단위로**
// 잘라서 들어가지 못한 항목 수를 사실대로 알려야 하고, 호출자는 실제로 전달된 것만 표시한다.
//
// 예전에는 전체를 렌더링한 뒤 자르고 배치 전체를 전달됨으로 표시했다. 메시지 뒤쪽은 소리 없이 사라지는데
// 전달 이력은 모두 성공으로 나와, 취약점이 그대로 사라지고 어디서도 알 수 없었다.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 한글 다이제스트 200건은 WeCom의 4096바이트를 반드시 크게 넘는다.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("본문 %d바이트가 상한 %d를 넘음", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("본문이 올바른 UTF-8이 아님")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 들어가야 함(0 < kept < %d), 실제값 %d", len(m.Items), kept)
	}
	// 머리는 이 메시지에 몇 건만 들었고 나머지가 몇 건인지 사실대로 밝혀야 한다. 그러지 않으면 읽는 사람이 머리의
	// 그 숫자를 전부로 안다.
	if !strings.Contains(body, "나머지") || !strings.Contains(body, "다음 메시지로 이어집니다") {
		t.Fatalf("머리에 이 메시지에 들지 않은 건수가 있어야 함:\n%s", body[:minInt(400, len(body))])
	}
	// 앞의 kept건만 들어 있어야 한다.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "취약점"+itoa(i+1)) {
			t.Fatalf("%d번째 항목이 이 메시지에 있어야 함:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "취약점"+itoa(kept+1)) {
		t.Fatalf("%d번째 항목이 나오면 안 됨(다음 배치에 속함)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 제한 없음
	if kept != len(m.Items) {
		t.Fatalf("길이 제한이 없으면 모두 유지해야 함, 실제값 kept=%d", kept)
	}
	if strings.Contains(body, "나머지") {
		t.Fatalf("잘리지 않았으면 잘림 안내가 나오면 안 됨:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 예산이 한 건도 담지 못할 만큼 작아도 한 건은 보내야 한다(마지막 자르기에 맡긴다).
	// 그러지 않으면 너무 긴 취약점 하나가 배치 전체를 영원히 막는다. 할당받을 때마다 들어가지 않아 매번 보내지 않는다.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("최소 1건은 유지해야 함, 실제값 %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("단건 메시지는 전달 1건을 알려야 함, 실제값 %d", kept)
	}
	// 빈 메시지에는 전달할 항목이 없다.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("빈 메시지는 0건을 알려야 함, 실제값 %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram은 **문자 수**로 길이를 제한한다. 바이트 기준을 쓰면 한글 메시지가 3분의 1로 줄어든다.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("본문 %d자가 상한 %d를 넘음", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 들어가야 함, 실제값 %d", kept)
	}
	if !strings.Contains(text, "다음 메시지로 이어집니다") {
		t.Fatalf("들지 않은 나머지가 있다고 밝혀야 함:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("카드에는 일부만 들어가야 함, 실제값 %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// 이 두 알림 채널은 본문을 자르지 않으므로 배치 전체를 전달됨으로 친다.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("전제 조건이 성립하지 않음")
	}
	// 렌더러의 반환값으로 간접 확인한다. markdownBody(0)는 제한이 없을 때 모두 유지한다.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("길이 제한이 없으면 모두 써야 함, 실제값 %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent 는 '신뢰할 수 없는 내용이 메시지 구조를 바꾸면 안 된다'의 회귀 테스트다.
// 제목과 요약은 모델 출력(모델은 테스트 대상의 응답을 읽는다)에서, 자산 이름은 테스트 대상의 URL에서 온다.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 결과에 반드시 나와야 함(이스케이프된 형태)
		wrong []string // 결과에 나오면 안 됨(이스케이프되지 않은 형태)
	}{
		{
			name: "제목의 줄바꿈 + 외부 링크",
			item: Item{
				Severity: "high",
				Name:     "로그인 페이지 SQL 인젝션\n[긴급: 여기를 눌러 계정 확인](http://attacker.tld)",
			},
			// 줄바꿈은 접어야 한다(그러지 않으면 새 목록 항목/인용 블록을 꾸밀 수 있다).
			// 대괄호와 소괄호는 이스케이프해야 한다(그러지 않으면 클릭할 수 있는 외부 링크가 된다).
			must:  []string{`\[긴급: 여기를 눌러 계정 확인\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[긴급", "\n\n[긴급"},
		},
		{
			name: "제목의 이미지 비콘",
			item: Item{
				Severity: "high",
				Name:     "취약점 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "자산 이름의 강조와 인용",
			item: Item{
				Severity: "high",
				Name:     "보통 제목",
				Assets:   []string{"a.com/*인젝션*>인용"},
			},
			must:  []string{`\*인젝션\*`, `\>`},
			wrong: []string{"*인젝션*"},
		},
		{
			name: "요약의 백틱과 세로 막대",
			item: Item{
				Severity: "high",
				Name:     "제목",
				Summary:  "`code` | 표",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 단건 모드의 writeItem은 세 markdown 알림 채널이 함께 쓰는 렌더링 경로다.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("이스케이프된 형태 %q이(가) 없음:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("이스케이프되지 않은 형태 %q이(가) 나옴(구조나 외부 링크를 넣는 데 쓰일 수 있음):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst 는 이스케이프 순서를 고정한다. 백슬래시를 가장 먼저 처리해야 한다.
// 그러지 않으면 뒤에서 더한 백슬래시를 한 번 더 감싸 출력에 이중 백슬래시가 나온다.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("이스케이프 순서 오류, 실제값 %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes 는 구체적인 회귀 하나를 고정한다.
// markdown 이스케이프가 Telegram HTML 출력으로 새면 안 된다(예전에 공유 제목 함수에
// 이스케이프를 더했더니 Telegram 메시지에 `\(1\)` 같은 백슬래시가 보였다).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *중요*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram 본문에 markdown 백슬래시 이스케이프가 나옴:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
