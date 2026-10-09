package notify

import (
	"net/url"
	"testing"
	"time"
)

// 서명 기준값은 이 패키지의 구현이 아니라 OpenSSL로 따로 계산했다.
// 그러지 않으면 '코드가 바뀌지 않았다'만 증명할 뿐 '알고리즘이 맞다'는 증명하지 못한다.
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("서명 실패: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("만든 주소를 파싱할 수 없음: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("서명 불일치\n기대값 %s\n실제값 %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("타임스탬프는 밀리초로 그대로 붙어야 함, 실제값 %q", q.Get("timestamp"))
	}
	// 원래 있던 query 파라미터(access_token)를 서명이 덮어쓰면 안 된다.
	if q.Get("access_token") != "tok" {
		t.Errorf("원래 query 파라미터가 사라짐, 실제값 %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("서명 불일치\n기대값 %s\n실제값 %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer 는 두 플랫폼의 알고리즘 차이를 고정한다. 둘은 파라미터 순서가 정확히 반대라
// (DingTalk key=secret, Feishu key=서명할 문자열), 다른 쪽을 베끼면 반드시 검증에 실패한다.
// 이 테스트는 나중에 리팩터링하면서 두 함수를 하나로 합치지 않게 한다.
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk과 Feishu 서명이 같음. 한쪽 알고리즘 구현이 틀렸다는 뜻이다")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 서명을 켜지 않은 봇: timestamp/sign 파라미터를 함부로 더하면 안 된다.
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret을 설정하지 않았으면 주소를 바꾸면 안 됨, 실제값 %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q은(는) 받아들여져야 함: %v", s, err)
		}
	}
	// file:// 같은 것은 통과시키면 안 된다. http.Client가 이를 다루는 방식이 예상 범위를 벗어난다.
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q은(는) 거부되어야 함", s)
		}
	}
}
