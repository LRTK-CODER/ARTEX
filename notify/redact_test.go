package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 이 파일은 불변식 테스트다. 알림 채널 구현에서 나오는 **어떤** 오류 텍스트도 자격 증명을 담으면 안 된다.
//
// 따로 파일을 둔 이유: 처음의 알림 채널 테스트는 성공 경로와 플랫폼 업무 오류만 다루고,
// 전송 계층 실패는 전혀 보지 않았다. 그런데 바로 전송 계층 오류(연결 거부/DNS 실패/시간 초과)가 가장 위험하다.
// http.Client.Do가 돌려주는 *url.Error는 **전체 URL**을 오류 텍스트에 넣는데, 이 기능의
// 알림 채널들은 자격 증명이 URL 안에 있다. 자격 증명이 이 문자열을 따라 네 출구로 흘러갔다.
//
//	notification_deliveries.last_error  → 평문 저장
//	GET /api/notify/deliveries 응답     → 알림 채널 설정의 마스킹을 우회해 브라우저로 돌려줌
//	서버 로그                           → 밖으로 보내 보관하는 경우가 많음
//	테스트 발송 API의 502 응답          → 프런트에 바로 뜸
//
// 그래서 함수 하나만 테스트하지 않고, 알림 채널마다 반드시 실패하는 요청을 실제로 한 번 보내 오류 텍스트에서
// 그 자격 증명을 찾을 수 없음을 단언한다.

// credentialCases 는 '자격 증명이 URL 안에 있는' 모든 알림 채널 형태를 다룬다.
// DingTalk/WeCom은 query에, Feishu는 경로 끝에, Telegram은 경로 중간에 있다.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk access_token이 query에 있음",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom key가 query에 있음",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu hook id가 경로 끝에 있음",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token이 경로 중간에 있음",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk 서명 키",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken 은 실제 자격 증명일 수 없는 센티넬 값으로, 오류 텍스트에서 이것을 찾는다.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials 가 핵심 불변식이다.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 반드시 실패하는 상대: 127.0.0.1:1에는 아무도 수신하지 않아 연결 거부 경로를 탄다.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "유출 탐침"}},
			})
			if err == nil {
				t.Fatal("닿을 수 없는 주소면 오류여야 함")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath 는 영구 실패 분기를 다룬다.
// URL 검사 실패, 플랫폼 업무 오류 등도 오류 텍스트를 밖으로 내보내므로 마찬가지로 자격 증명을 담으면 안 된다.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// 주소에 자격 증명이 있지만 형식이 잘못됨 → validateHTTPURL / url.Parse 분기를 탄다.
		{"DingTalk 주소 잘못됨", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"WeCom 주소 잘못됨", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"Feishu 주소 잘못됨", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API 주소 잘못됨", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"범용 Webhook 주소 잘못됨", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("잘못된 설정이면 오류여야 함")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("오류 텍스트가 자격 증명 %q을(를) 유출함:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q, 기대값 %q", in, got, want)
		}
		// 가린 결과에는 원래 주소의 경로/쿼리 조각이 하나도 남으면 안 된다.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("가린 뒤에도 경로/쿼리 조각 %q이(가) 남음: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 파싱할 수 없는 입력은 원래 문자열을 절대 돌려주지 않는다.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("파싱할 수 없는 입력 %q이(가) %q로 돌아옴", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL 은 *url.Error라는 구체 타입을 직접 본다.
// http.Client.Do의 반환 타입이자 유출이 처음 일어나는 곳이다.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("문제를 찾을 수 있게 host를 남겨야 함, 실제값 %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("문제를 찾을 수 있게 바탕 원인을 남겨야 함, 실제값 %q", got)
	}
	// Op도 남겨야 한다(POST인지 GET인지가 문제를 찾는 데 의미가 있다).
	if !strings.Contains(got, "Post") {
		t.Errorf("동작 이름을 남겨야 함, 실제값 %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback 은 대비 경로다. *url.Error가 아닌 사용자 지정 오류
// (리다이렉트 정책이 돌려준 오류 등) 안의 주소도 떼어 내야 한다.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("다른 호스트로의 리다이렉트를 거부했습니다(a.example → http://b.example/bot%s/send)", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("주소를 가린 형태로 바꿔야 함, 실제값 %q", got)
	}
	// 주소가 없는 텍스트는 그대로 둔다.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("주소가 없는 텍스트를 바꾸면 안 됨")
	}
}

// TestCrossHostRedirectRefused 는 '자격 증명이 URL에 있음 + 호스트를 넘는 이동을 따라감 = 자격 증명을 넘겨줌'을 다룬다.
// httptest의 두 서비스는 127.0.0.1의 서로 다른 포트에서 수신한다. 포트가 다르면 Host도 달라
// 호스트를 넘는 이동이 된다.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("호스트를 넘는 리다이렉트는 거부되어야 함")
	}
	if hit {
		t.Fatal("이동 대상에 접근함. 자격 증명이 리다이렉트를 따라 유출됐다")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed 는 반대 방향 테스트다. 같은 호스트 안의 이동(끝에 슬래시 보충 등)은 여전히 되어야 한다.
// 그러지 않으면 정상 작업 흐름까지 함께 막는다.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 같은 호스트, 같은 포트로의 이동.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("같은 호스트 리다이렉트는 거부하면 안 됨: %v", err)
	}
}
