package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// 이 파일은 서로 관련된 두 가지 강화를 다룬다.
//   ① 전달 주소로 서버를 발판 삼아 내부망 / 클라우드 메타데이터를 치면 안 된다(SSRF)
//   ② 주소 검사의 오류 메시지가 주소 안의 자격 증명을 실어 내면 안 된다
//
// 테스트 환경: 이 패키지의 많은 테스트가 127.0.0.1의 httptest 가짜 수신 측을 쓰는데, 방어는 기본적으로 이를
// 막는다. 그래서 TestMain에서 AllowLocalTargetsEnv를 한꺼번에 켜고, 아래의 SSRF 테스트는 각각
// 이것을 명시적으로 지워 **기본 거부** 동작을 단언한다.

func TestMain(m *testing.M) {
	// 일반 테스트가 로컬 가짜 수신 측에 연결할 수 있게 한다. SSRF 테스트는 스스로 잠시 비운다.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault 는 SSRF 방어의 핵심 단언이다.
// 기본 설정에서 루프백 주소로의 전달은 **연결 계층**에서 거부되어야 한다.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구를 끔 = 기본 동작
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("기본적으로 루프백 주소로 전달하면 안 됨")
	}
	if hit {
		t.Fatal("요청이 로컬 서비스에 닿음. 방어가 작동하지 않았다")
	}
	// 오류 메시지가 허용하는 방법을 안내해야 한다(로컬 SMTP 릴레이는 정상 설정이다).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 메시지가 명시적으로 허용하는 방법을 설명해야 함: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn 은 반대 방향 테스트다. 명시적으로 켜면 쓸 수 있어야 한다.
// 그러지 않으면 로컬 postfix / 내부망 릴레이 같은 정상 배포가 일괄로 막힌다.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("명시적으로 허용하면 전달할 수 있어야 함: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // 클라우드 메타데이터 엔드포인트. 이 함수가 있는 주된 이유다
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped 형태는 되돌린 뒤 판단해야 한다. 그러지 않으면 우회 경로다
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s은(는) 거부되어야 함", s)
		}
	}
	// RFC1918 사설망은 **일부러 허용한다**. 내부망의 자체 Mattermost / SMTP 릴레이는 흔한 정상 사용이다.
	// 이 단언이 그 선택을 고정한다. 나중에 누가 무심코 사설망 판단을 더하면 여기서 실패해,
	// 소리 없이 여러 배포를 망가뜨리는 대신 의식적인 결정을 하게 만든다.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s은(는) 허용되어야 함(사설망은 흔한 정상 전달 대상)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets 는 설정 단계의 사전 안내를 다룬다.
// 리터럴 IP는 첫 전달이 실패할 때까지 기다리지 않고 저장할 때 거부해야 한다.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s은(는) 설정 단계에서 거부되어야 함", raw)
		}
	}
	// 공인 주소와 사설 주소는 그대로 통과한다(사설망은 연결 단계에 맡기고, 그곳에서도 막지 않는다).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s은(는) 검사를 통과해야 함: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials 는 감사에서 지적된, 지난번에 빠뜨린 분기다.
//
// url.Parse가 **실패**하면 *url.Error를 돌려주고 그 Error()에는 원래 주소 전체가 들어 있다. 지난번에는
// http.Client.Do가 돌려준 오류만 가리고 여기를 빠뜨렸다. 그때 보충한 '영구 실패 경로' 테스트
// (file://, gopher://, ftp://)는 사실 모두 url.Parse가 파싱에 성공해 scheme 분기를 탔으므로,
// 모두 통과해도 이 경로가 안전하다는 증명이 되지 않았다. 거짓 보증이었다.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 잘못된 퍼센트 이스케이프
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // 숫자가 아닌 포트
		"http://[::1?access_token=" + leakProbeToken,                  // 괄호 짝이 맞지 않음
	}
	for _, raw := range cases {
		// 먼저 이 입력이 **정말로** url.Parse를 실패시키는지 확인한다. 이 단계가 없으면 테스트가
		// 전혀 모르는 사이에 다른 분기를 탈 수 있다(지난번의 거짓 보증이 이렇게 생겼다).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q은(는) 파싱에 실패해야 함. 그러지 않으면 이 테스트가 목표 분기를 다루지 못함", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q은(는) 검사에 실패해야 함", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// 알림 채널 계층의 감싸기도 주소를 실어 내지 않는지 확인한다.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("잘못된 주소는 검사에 실패해야 함")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault 는 SMTP 알림 채널의 연결 방어를 다룬다.
//
// 이메일 알림 채널은 예전에 맨 net.Dialer를 써서 SSRF 방어 전체의 유일한 구멍이었다. host를
// 169.254.169.254나 127.0.0.1로 넣으면 바로 연결되고, smtp.NewClient 핸드셰이크가 실패할 때
// 상대가 돌려준 줄을 오류에 담아 last_error를 거쳐 전달 이력 API로 돌려줬다. 다른 알림 채널에서
// 이미 막은 반쯤 눈먼 읽기 수단이고, '연결 거부 vs 시간 초과'의 소요 시간 차이로 포트를 탐지할 수도 있었다.
//
// 이 패키지의 TestMain은 AllowLocalTargetsEnv를 전역으로 켠다(많은 테스트가 127.0.0.1의
// 가짜 수신 측을 쓴다). 그래서 이 테스트는 스스로 그것을 지워야 한다. 그러지 않으면 방어가 있든 없든 통과하고,
// 그것이 바로 이 구멍이 처음에 어떤 테스트에도 걸리지 않은 이유다.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구를 끔 = 기본 동작
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("기본적으로 이메일을 루프백 주소로 전달하면 안 됨")
	}
	// 연결이 아예 만들어지면 안 된다. 방어가 Control 훅에서 막아 EHLO는 절대 나가지 않는다.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP 세션이 만들어짐. 방어가 작동하지 않았다")
	}
	// 오류 메시지가 허용하는 방법을 안내해야 한다(로컬 postfix 릴레이는 정상 설정이다).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 메시지가 명시적으로 허용하는 방법을 설명해야 함: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn 은 짝이 되는 반대 방향 테스트다. 명시적으로 켜면
// 정상적으로 전달해야 한다. 내부망 자체 SMTP / 로컬 릴레이는 아주 흔한 배포라 방어가 일괄로 막으면 안 된다.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("명시적으로 허용하면 로컬 SMTP로 전달할 수 있어야 함: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO가 보이지 않음. 세션이 실제로 만들어지지 않았다")
	}
}
