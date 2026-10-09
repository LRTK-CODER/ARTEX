package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout은 각각 연결 수립과 SMTP 세션 전체를 제한한다.
// net/smtp 자체에는 시간 초과 장치가 전혀 없어서, 이 둘을 두지 않으면 멈춘 상대 하나가
// 전달 goroutine을 영원히 붙잡는다. dispatcher는 goroutine 하나로 차례로 처리하므로,
// 알림 시스템 전체가 멈추는 것과 같다.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel 은 SMTP 이메일 전달을 구현한다.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// 이메일에는 플랫폼 속도 제한이 없지만 알림이 넘치게 써서는 안 되므로 넉넉한 기본값을 준다.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// 비밀번호만 마스킹한다. SMTP 호스트, 계정, 받는 사람은 비밀이 아니고, 마스킹하면 편집만 번거로워진다.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port는 비밀번호를 어느 서버에 넘길지 정하고, tls는 암호화해서 보낼지 정한다. 셋 중 하나라도 바뀌면
// 비밀번호를 다시 입력해야 한다. 덕분에 'TLS 끄기'도 자격 증명을 명시적으로 함께 보내야 하고, 무심코 바꿀 수 없다.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP 서버 주소를 입력하세요")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 포트는 1-65535 사이여야 합니다")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("보낸 사람 주소를 입력하세요")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("받는 사람 주소를 하나 이상 입력하세요")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: 상대가 지원하면 올린다. 평문 세션에서는 자격 증명을 보낼 수 없다(아래 auth 설명 참고).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 실패: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth는 암호화되지 않은 연결로 자격 증명을 보내기를 거부한다(대상이 localhost가 아니면).
			// 이것은 **올바른** 보안 동작이라 우회하면 안 되지만, 이유를 알기 쉽게 옮겨 줘야 한다.
			// 그러지 않으면 사용자는 'unencrypted connection'만 보고 무엇을 해야 할지 모른다.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("연결이 암호화되지 않아 자격 증명을 보내지 않았습니다. TLS를 쓰도록 설정하세요. 465 포트로 바꾸고 '암시적 TLS'를 켜면 됩니다 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 인증 실패: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("보낸 사람 %s 거부됨", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("받는 사람 %s 거부됨", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 실패: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("이메일 본문 쓰기 실패: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("이메일 제출 실패: %w", err)
	}
	// Quit이 실패해도 '서버가 이메일을 받았다'는 사실은 바뀌지 않으므로 그 오류는 무시한다.
	_ = client.Quit()
	// 이메일은 길이를 자르지 않으므로(HTML 본문을 모두 보낸다) 배치 전체를 전달됨으로 친다.
	return len(m.Items), nil
}

// emailDial 은 SMTP 연결을 수립한다.
//
// implicitTLS=true면 465처럼 '연결하자마자 TLS'인 방식을 쓰고, false면 25/587에 평문으로 연결한 뒤
// STARTTLS를 한다. 둘을 섞으면 안 된다. 465 포트에 평문 greeting을 보내면 바로 끊긴다.
//
// 세션 기한은 **연결을 만들 때** 정한다(나중에 덧붙이지 않는다). net/smtp의 Client는 바탕
// 연결을 내보내지 않는 필드에 숨겨 밖에서 얻을 수 없다. 연결을 넘긴 뒤에는 미리 정한 deadline에만
// 기댈 수 있다. 이렇게 하면 핸드셰이크 단계의 멈춤도 함께 막는다.
// Control에 blockInternalDial을 걸어 HTTP 계열 알림 채널과 같은 방어를 쓴다. 걸지 않으면 SMTP가
// SSRF 방어 전체의 구멍이 된다. host에 169.254.169.254나 127.0.0.1을 넣으면 바로 연결되고,
// smtp.NewClient 핸드셰이크가 실패할 때 상대가 돌려준 줄을 오류에 담아 last_error를 거쳐
// 전달 이력 API로 돌려주므로 반쯤 눈먼 읽기 수단이 된다. '연결 거부 vs 시간 초과'의 소요 시간 차이로
// 포트를 탐지할 수도 있다. 연결 단계가 최종 적용 지점이고 DNS 리바인딩도 막는다.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 서버 연결 실패: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 핸드셰이크 실패: %w", err)
	}
	return client, nil
}

// smtpStageError 는 SMTP 응답 코드에 따라 어느 단계의 실패를 '재시도 가능'과 '영구 실패'로 나눈다.
//
// 꼭 나눠야 하는 이유: SMTP의 4xx와 5xx는 뜻이 완전히 다르다.
//   - 4xx(450 그레이리스트, 451 로컬 오류, 452 저장 공간 부족)는 **일시적** 거부이고,
//     정석은 잠시 뒤 재시도하는 것이다. 특히 그레이리스트는 첫 전달 때 거의 늘 만난다.
//   - 5xx(550 사용자 없음, 553 잘못된 주소)는 영구 거부라 재시도해도 소용없다.
//
// 모두 영구 실패로 판정하면, 그레이리스트를 쓰는 메일 서버 하나 때문에 **모든** 알림이 첫
// 시도 뒤 failed가 된다. 이런 실패야말로 자동 재시도가 가장 쓸모 있는 경우다.
// 응답 코드는 오류 텍스트의 앞 세 자리 숫자에서 얻는다. 코드를 얻지 못하면 재시도 가능으로 다룬다(한 번 더
// 시도하는 편이, 파싱하지 못했다고 일시적일 수 있는 장애를 영구 실패로 판정하는 것보다 낫다).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode 는 SMTP 오류 텍스트 앞의 세 자리 응답 코드를 얻는다. 얻지 못하면 0을 돌려준다.
// net/smtp는 오류 코드 필드를 내보내지 않아 텍스트에서 얻을 수밖에 없다. 형식은 '450 4.7.1 ...'이다.
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage 는 완전한 RFC 5322 이메일을 조립한다.
//
// 본문을 base64로 인코딩하는 이유는 둘이다. 하나는 SMTP가 한 줄을 1000바이트 이하로 정하는데 HTML
// 본문(특히 다이제스트 이메일)은 줄이 쉽게 길어지기 때문이고, 다른 하나는 base64에는 "."으로 시작하는
// 줄이 생기지 않아 SMTP 점 이스케이프를 신경 쓰지 않아도 되기 때문이다.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 한글 등 비ASCII 제목은 RFC 2047로 인코딩해야 한다. 그러지 않으면 클라이언트에서 글자가 깨진다.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// 이메일에는 고정 길이 상한이 없으므로 본문을 자르지 않는다.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64는 76자마다 줄을 바꾼다. RFC 2045에 맞춘다.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
