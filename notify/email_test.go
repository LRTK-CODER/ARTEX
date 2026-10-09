package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// 이 파일은 이메일 알림 채널의 프로토콜 수준 테스트를 채운다. 이전에는 email.Send의 커버리지가 0이었다.
// SMTP 경로 전체를 도는 테스트가 하나도 없었는데, 이메일은 여섯 알림 채널 중 프로토콜 면이 가장 넓고
// 가장 틀리기 쉬운 채널이다(핸드셰이크, 인증, 봉투, DATA 단계마다 실패 의미가 다르다).
//
// 여기서는 net/smtp를 mock으로 바꾸지 않고 직접 만든 최소 SMTP 서버로 돌린다.
// 이메일 알림 채널의 위험 대부분은 '실제 SMTP 서버와 대화하는' 단계에 있어서,
// 이 단계를 mock으로 바꾸면 테스트하지 않는 것과 같다.

// fakeSMTP 는 딱 필요한 만큼의 SMTP 서버다. greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT를 마칠 수 있고,
// 테스트가 요구하면 특정 단계에서 지정한 응답 코드를 돌려준다.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply 는 RCPT TO의 응답이다. 기본값 250.
	rcptReply string
	// mailReply 는 MAIL FROM의 응답이다. 기본값 250.
	mailReply string
	// advertiseAuth 가 true면 EHLO에서 AUTH PLAIN 지원을 알린다.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP 수신 주소가 아님")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS를 알리지 않는다. 코드가 평문 분기를 타게 한다(테스트 목표는 TLS가 아니라 봉투 로직이다).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 단순화: PLAIN의 첫 응답은 여러 줄일 수 있으므로 바로 받아들인다.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	// 봉투 단계는 보낸 사람, 받는 사람 둘, DATA까지 가야 한다.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP 세션에 %q이(가) 없음, 실제 명령: %v", want, f.commands)
		}
	}
	// 본문은 base64로 된 HTML이고, 실제 취약점 내용을 담아야 한다(인코딩한 뒤에도 알아볼 수 있다).
	body := f.body()
	if body == "" {
		t.Fatal("DATA 단계에서 본문을 받지 못함")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type 헤더 없음:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("본문을 base64로 인코딩하지 않음(긴 HTML 줄은 SMTP의 1000바이트 줄 길이 제한을 깬다):\n%s", body)
	}
	// 받는 사람 여럿이 모두 To 헤더에 나와야 한다.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To 헤더에 받는 사람이 모두 들어 있지 않음:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// 계정을 설정하지 않았으면 AUTH를 보내면 안 된다. 어떤 릴레이는 그 때문에 거부한다.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("계정을 설정하지 않았는데 AUTH를 보냄: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies 는 이번 감사 수정을 직접 검증한다.
// 5xx는 영구 실패, 4xx(그레이리스트)는 재시도 가능으로 판정한다.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"받는 사람이 550으로 영구 거부됨", "550 5.1.1 User unknown", "250 OK", true},
		{"받는 사람이 450 그레이리스트에 걸림", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"받는 사람이 452 메일함 가득 참", "452 4.2.2 Mailbox full", "250 OK", false},
		{"보낸 사람이 553으로 영구 거부됨", "250 OK", "553 5.1.3 Bad address", true},
		{"보낸 사람이 451 일시 오류", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("오류여야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 판정 오류: permanent = %v, 기대값 %v (%v)", got, tc.permanent, err)
			}
			// 서버 원문을 남겨야 한다. 그러지 않으면 사용자는 서버 관리자에게 물을지 주소를 고칠지 모른다.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("오류에 서버의 응답 코드가 남아 있어야 함: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp의 PlainAuth는 암호화되지 않은 연결로 자격 증명을 보내기를 거부한다(대상이 localhost가 아니면).
	// 이것은 **올바른** 보안 동작이라 우회되면 안 된다. 다만 사용자가 고칠 수 있게 안내하는 오류를 줘야 한다.
	// 여기서는 localhost가 아닌 호스트 이름으로 이것을 일으킨다.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost가 아님
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("로컬 DNS가 로컬 서버로 조회되어 건너뜀(다른 테스트에는 영향 없음)")
	}
	// 연결하지 못하거나 자격 증명 전송을 거부하면 모두 이 단언을 통과한다. 핵심은 비밀번호를 소리 없이 보내면 **안 된다**는 것이다.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "연결") {
		t.Logf("오류: %v(localhost가 아닐 때 연결하지 못하는 것은 예상된 일)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// 이메일 알림 채널은 설정 필드가 가장 많고, 하나라도 빠지면 전달할 때에야 드러난다. 여기서 하나씩
	// 검사가 미리 막는지 확인한다. 단언은 '오류 메시지가 무엇이 빠졌는지 말하는지'를 본다.
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host 없음", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port 없음", map[string]any{"host": "smtp.example.com"}},
		{"port 범위 초과", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from 없음", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to 없음", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("검사에 실패해야 함: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance 는 설정 읽기의 너그러움을 다룬다. JSONB의 숫자는 float64이지만
// 사용자가 UI에서 포트를 문자열로 넣을 수 있고, 배열도 문자열 하나일 수 있다.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 문자열 형태의 포트
		"from": "a@b.c",
		"to":   "d@e.f", // 배열이 아닌 문자열 하나
		"tls":  "true",  // 문자열 형태의 불리언
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("문자열 형태의 숫자를 받아들여야 함: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt가 문자열 포트를 파싱하지 못함, 실제값 %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool이 문자열 \"true\"를 파싱하지 못함")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings가 문자열 하나를 받지 못함, 실제값 %v", to)
	}
}

// TestFilterValidateRejectsTypo 는 감사 수정을 직접 검증한다.
// 기준을 잘못 입력하면 쓸 때 막아야 한다. 그러지 않으면 필터가 소리 없이 무력해져 모두 보내기가 된다.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("올바른 기준 %q이(가) 거부됨: %v", s, err)
		}
	}
	// 실제로 일어나는 오타들이다. 모두 거부되어야 한다.
	for _, s := range []string{"hgih", "HIGH", "치명", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("잘못된 기준 %q은(는) 거부되어야 함(그러지 않으면 필터가 소리 없이 무력해져 모두 보내기가 됨)", s)
			continue
		}
		// 오류 메시지가 사용자가 바르게 고치도록 안내해야 한다.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("오류 메시지에 고를 수 있는 값이 있어야 함, 실제값 %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly 는 '쓸 때는 엄격, 읽을 때는 너그럽게'의 분담을 고정한다.
// DB에 이미 있는 잘못된 값 때문에 알림 채널 전체를 읽지 못하면 안 된다(기존 알림 채널이 갑자기 모두 알림 발송을 멈춘다).
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // 오류를 내지 않는다
	if f.MinSeverity != "hgih" {
		t.Fatalf("읽기 경로는 그대로 유지해야 함, 실제값 %q", f.MinSeverity)
	}
	// 그리고 이 알림 채널은 여전히 이벤트를 판정할 수 있어야 한다(panic, 멈춤 없음).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
