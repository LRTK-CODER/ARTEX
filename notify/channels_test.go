package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg 는 따옴표와 줄바꿈이 든 단건 메시지를 만든다. `"`와 `\n`이 든 제목/요약을 일부러 쓴다.
// 템플릿 끼워 넣기가 잘못된 JSON을 만들기 가장 쉬운 입력이다.
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `로그인 페이지 "SQL 인젝션" 위험`,
			VulnClass: "SQL 인젝션",
			Severity:  "high",
			Summary:   "파라미터 id\n필터링 없음 인젝션 발생",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg 는 다이제스트 메시지 한 배치를 만든다.
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "취약점" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "반사형 크로스 사이트 스크립팅",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost 는 가짜 수신 측을 띄우고, 받은 요청 본문과 헤더를 단언 함수에 넘긴다.
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("요청 본문이 올바른 JSON이 아님: %v\n원문: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("상세 링크가 있으면 actionCard를 보내야 함, 실제값 %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("상세 링크 유실: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("다이제스트 메시지는 markdown을 보내야 함, 실제값 %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "최근 30분") {
			t.Errorf("다이제스트 본문에 시간 범위가 없음: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent 는 'HTTP 200인데 errcode가 0이 아님'의 판정을 고정한다.
// errcode를 검사하지 않으면 전달 실패가 성공으로 기록된다. 중국 IM 플랫폼들에 공통된 함정이다.
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode가 0이 아니면 오류여야 함")
	}
	if !IsPermanent(err) {
		t.Fatalf("키워드 불일치는 설정 오류라 영구 실패로 표시해야 함, 실제값 %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("오류 메시지에 플랫폼 오류 코드가 있어야 함, 실제값 %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("자른 뒤 올바른 UTF-8이 아님. WeCom은 메시지 전체를 거부한다")
		}
	})
	// 충분히 긴 한글 다이제스트를 만들어 반드시 4096바이트를 넘게 한다.
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("본문 %d바이트가 WeCom 상한 %d를 넘음", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("본문이 비어 있음")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009는 굴러가는 창의 속도 제한이라 재시도할 수 있어야 함, 실제값 %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000은 key가 잘못된 것이라 재시도해도 낫지 않으므로 영구 실패여야 함, 실제값 %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("인터랙티브 카드를 보내야 함, 실제값 %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high 심각도는 orange 색이어야 함, 실제값 %v", header["template"])
		}
		// secret을 설정했으면 서명 파라미터가 있어야 한다. 없으면 Feishu가 19021로 거부한다.
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("서명 파라미터 없음: %v", body)
		}
		// 카드 요소에 취약점 상세를 가리키는 url을 가진 버튼이 있어야 한다.
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("카드에 상세 페이지를 가리키는 버튼이 없음")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("secret을 설정하지 않았으면 서명 파라미터가 없어야 함: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML 파싱 모드를 써야 함, 실제값 %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// 제목과 요약은 테스트 대상/모델 출력에서 오므로 신뢰할 수 없는 내용이다.
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML을 이스케이프하지 않아 인젝션이 가능함: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("이스케이프된 엔티티가 있어야 함, 실제값 %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("&를 이스케이프하지 않음, 실제값 %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429는 재시도할 수 있어야 함, 실제값 %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403은 설정 문제라 영구 실패여야 함, 실제값 %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// 이것이 기본 템플릿이 있는 이유다. 제목에 따옴표와 줄바꿈이 있으면 단순한
	// `"title": "{{.Title}}"` 방식은 모두 잘못된 JSON을 만든다. {{json .}}만 그렇지 않다.
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 높음] 로그인 페이지 "SQL 인젝션" 위험` {
			t.Errorf("제목이 제대로 복원되지 않음: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items 개수 = %d, 기대값 1", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "파라미터 id\n필터링 없음 인젝션 발생" {
			t.Errorf("요약이 제대로 복원되지 않음: %v", it["summary"])
		}
		// 숫자는 문자열이 아니라 JSON 숫자여야 한다(json:"...,string" 같은 방식이 이 함정에 빠진다).
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id는 숫자여야 함, 실제값 %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("사용자 지정 헤더 유실: %v", r.Header)
		}
		if body["msg"] != "3건" {
			t.Errorf("사용자 지정 템플릿 렌더링 오류: %v", body["msg"])
		}
		if body["first"] != "취약점1" {
			t.Errorf("range 추출 오류: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d건" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("렌더링 결과가 JSON이 아니면 영구 실패여야 함(템플릿을 잘못 쓴 것이라 재시도해도 소용없음), 실제값 %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("%d번째 설정은 거부되어야 함: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("이메일 조립 실패: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From 헤더 오류:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To 헤더 오류:\n%s", msg)
	}
	// 한글 등 비ASCII 제목은 RFC 2047로 인코딩해야 한다. 그러지 않으면 클라이언트에서 글자가 깨진다.
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("제목을 RFC 2047로 인코딩하지 않음:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("제목을 디코딩할 수 없음: %v", err)
	} else if !strings.Contains(dec, "SQL 인젝션") {
		t.Fatalf("디코딩한 제목 내용 오류: %q", dec)
	}

	// 본문은 base64이고, 풀면 올바른 HTML이어야 한다.
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("이메일에 헤더/본문 구분이 없음")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("본문 base64 디코딩 실패: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("본문이 HTML이 아님: %.80s", html)
	}
	// 제목은 텍스트 위치에 그대로 나온다. HTML 텍스트 내용의 큰따옴표는 올바른 문자라 이스케이프할 필요가 없다.
	// 여기서 '그대로 유지'를 단언하는 것은 나중에 누가 실수로 따옴표 이스케이프를 한 겹 더해
	// 따옴표가 &quot;로 보이게 되는 것을 막으려는 것이다.
	if !strings.Contains(html, `"SQL 인젝션"`) {
		t.Fatalf("텍스트 위치의 제목 따옴표는 그대로 남아야 함: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection 은 이메일 본문에서 정말 막아야 할 인젝션을 다룬다.
// 취약점 제목과 요약은 테스트 대상과 모델 출력에서 오므로 신뢰할 수 없다. 텍스트 위치에서는
// & < >를 이스케이프해야 하고(그러지 않으면 태그를 넣을 수 있다), 속성 위치에서는 따옴표도 이스케이프해야 한다(그러지 않으면 href를 닫을 수 있다).
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("제목을 이스케이프하지 않아 태그를 넣을 수 있음: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("이스케이프된 엔티티가 있어야 함: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("&와 >를 이스케이프하지 않음: %s", html)
	}
	// 상세 링크는 관리자가 설정하는 public_base_url이라 비교적 믿을 만하지만, 속성 위치에서는 여전히
	// 따옴표를 이스케이프해야 한다. 그러지 않으면 따옴표가 든 주소가 href를 닫고 이벤트 핸들러를 넣는다.
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 속성을 제대로 이스케이프하지 않음: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("속성 위치의 따옴표는 이스케이프되어야 함: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("%s 헤더를 찾지 못함", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 검사 오류는 설정하는 사람에게 그대로 보이므로, 막연한 '설정이 잘못됐습니다'가 아니라 무엇이 빠졌는지 분명히 말해야 한다.
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "포트"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "받는 사람"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("알림 채널 %s이(가) 등록되지 않음", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s 설정 %v은(는) 검사에 실패해야 함", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s의 오류 메시지에 %q이(가) 있어야 함, 실제값 %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification 은 SMTP 4xx/5xx의 뜻 구분을 고정한다.
// 4xx도 영구 실패로 판정하면 그레이리스트를 쓰는 메일 서버 하나 때문에 모든 알림이 첫
// 시도 뒤 failed가 된다. 그레이리스트야말로 자동 재시도가 가장 쓸모 있는 경우다.
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 응답 코드를 얻지 못하면 '재시도 가능'으로 다룬다. 한 번 더 시도하는 편이, 일시적일 수 있는
		// 장애를 영구 실패로 판정하는 것보다 낫다.
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("받는 사람 거부됨", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("응답 %q: permanent = %v, 기대값 %v", tc.reply, got, tc.permanent)
		}
		// 어떻게 분류하든 원문은 사용자가 원인을 찾도록 남겨야 한다.
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("응답 %q의 원문이 버려짐: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 여섯 알림 채널 중 하나도 빠지면 안 된다. 하나가 빠지면 UI 드롭다운에서 소리 없이 사라진다.
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("알림 채널 수 = %d, 기대값 %d: %v", len(got), len(want), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("알림 채널 %s이(가) 등록되지 않음", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("알림 채널 %s의 Kind()가 등록 키와 다름", k)
		}
	}
	if ValidKind("nope") {
		t.Error("등록되지 않은 유형은 검사를 통과하면 안 됨")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("영구 실패로 알아봐야 함")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("오류 메시지에 바탕 오류가 그대로 있어야 함: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil)은 nil을 돌려줘야 함")
	}
	if IsPermanent(nil) {
		t.Fatal("nil은 영구 실패가 아님")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
