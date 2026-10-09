package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets는 루프백 / 링크 로컬 주소로 메시지 전달을 허용할지 정한다.
//
// 기본값은 거부다. 이 주소 대역은 IM 봇이나 공인 메일 서버가 있을 곳이 아닌데, 여기로
// 닿는 대상은 민감하다. 같은 기기의 다른 서비스 관리 포트와 클라우드 환경의 메타데이터 엔드포인트
// (169.254.169.254, 인스턴스 자격 증명을 읽을 수 있다)다. 전달 주소는 관리자가 설정하지만, XSS/CSRF로
// 빌려 쓴 관리 세션이나 같은 JWT를 쓰는 두 번째 사람이 설정을 바꿔 응답 내용을 읽어 올 수 있다.
// doJSON은 4xx/5xx 응답 본문의 앞 200바이트를 last_error에 쓰고, 전달 이력 API가
// 그것을 돌려주므로, 반쯤 눈먼 읽기 수단이 된다.
//
// 하지만 '로컬 SMTP 릴레이'(127.0.0.1:25의 postfix)는 자체 메일 구축에서 흔한 설정이라
// 일괄로 막으면 사람이 막힌다. 그래서 하드코딩으로 허용하지 않고 명시적인 탈출구를 남긴다.
// ARTEX_NOTIFY_ALLOW_LOCAL=1로 설정하면 허용한다.
//
// AllowLocalTargetsEnv로 내보내는 것은 테스트가 이것을 분명히 켤 수 있게 하려는 것이다. 이 패키지와 server 패키지의
// 테스트는 127.0.0.1의 httptest 가짜 수신 측을 많이 쓰므로, 켜지 않으면 모두 방어에 막힌다.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP 는 대상 IP가 '기본적으로 전달을 허용하지 않는' 주소 대역에 속하는지 알려 준다.
//
// 루프백, 링크 로컬(클라우드 메타데이터 169.254.169.254 포함), 지정되지 않은 주소, 멀티캐스트만 거부한다.
// RFC1918 사설망은 **거부하지 않는다**. 내부망의 자체 Mattermost / SMTP 릴레이는 아주 흔한 정상 사용이라
// 함께 막으면 실제 환경에서 기능을 쓸 수 없다. 이 선택은 일부러 한 것이다.
// 방어는 정말 민감한 대상을 막되, 정상 배포까지 망가뜨려서는 안 된다.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6(::ffff:127.0.0.1)는 IPv4로 되돌린 뒤 판단해야 한다. 그러지 않으면 검사를 우회한다.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial 은 http.Transport 다이얼러의 Control 훅으로, **연결을 만들 때**
// 대상 주소를 검사한다.
//
// 설정을 저장할 때만 검사하지 않고 연결 단계에 두는 이유: 여기가 최종 적용 지점이다.
// 설정 검사를 우회하는 두 경우를 함께 막는다. DNS 리바인딩(검사할 때는 공인 IP로 조회되고
// 실제 연결할 때는 내부망으로 조회됨)과 리다이렉트(호스트를 넘는 이동은 이미 거부하지만, 같은 호스트 안의 이동이
// 경로를 다른 곳으로 돌릴 수 있다)다.
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("대상 주소 %q을(를) 파싱할 수 없습니다", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("로컬/링크 로컬 주소 %s(으)로는 전달하지 않습니다(로컬 서비스로 꼭 전달해야 하면 %s=1로 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport 는 기본 Transport에 연결 방어 하나만 더한다.
// Clone으로 기본 튜닝(연결 풀, HTTP/2, 시간 초과, proxy 등)을 모두 유지해
// 검사 하나를 더하려고 다른 동작을 바꾸지 않는다.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient 는 모든 알림 채널 전달이 함께 쓰는 클라이언트다.
//
// 프로젝트의 전역 아웃바운드 프록시(server 쪽 GlobalProxy)를 일부러 다시 쓰지 **않는다**. 그 프록시는 침투 테스트
// 대상 트래픽용이라 불안정한 터널인 경우가 많고, 알림의 가용성이 대상 네트워크의 흔들림에 묶여서는 안 된다.
// IM 알림 발송은 직접 연결하면 된다. 시간 초과는 15초다. 이보다 느린 상대는 사실상 장애 상태다.
//
// 호스트를 넘는 리다이렉트를 거부한다. 이 기능의 전달 주소는 모두 '고정된 endpoint 하나' 형태라 보통
// 다른 호스트로 리다이렉트하지 않는다. 그런데 이들의 자격 증명(DingTalk access_token, WeCom key, Telegram
// bot token)은 **URL 안에 있어서**, 호스트를 넘는 이동을 따라가면 자격 증명을 리다이렉트 대상에 넘기는 셈이다. 같은 호스트
// 안의 이동(끝에 슬래시 보충 등)은 여전히 허용한다.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("리다이렉트가 너무 많습니다")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("다른 호스트로의 리다이렉트를 거부했습니다(%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit 은 응답 본문을 읽는 크기를 제한한다. 상대가 이상하면 아주 큰 내용을 돌려줄 수 있는데, 필요한 것은
// 전달 이력에 보여 줄 오류 코드와 짧은 오류 설명뿐이다.
const respBodyLimit = 8 << 10

// doJSON 은 요청을 한 번 보내고 응답 본문(길이 제한됨)을 돌려준다.
//
// payload가 nil이면 빈 body를 보낸다(GET이나 플랫폼이 body를 요구하지 않을 때).
// headers의 키와 값은 그대로 붙인다. 범용 웹훅의 사용자 지정 헤더에 쓴다.
//
// 오류 분류가 이 함수의 핵심 책임이다. 네트워크 계층 실패와 5xx/408/429는 '재시도 가능'으로,
// 나머지 4xx는 '영구 실패'로 나눈다. 403을 재시도해 봐야 같은 오류가 로그에 세 번 찍힐 뿐이다.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// 직렬화 실패는 로컬 버그(설정 필드 타입 오류)라 재시도해도 나아지지 않는다.
			return nil, Permanent(fmt.Errorf("요청 본문 만들기 실패: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL이 잘못됐다. 대개 사용자가 주소를 잘못 넣은 것이라 영구 실패다.
		// 여기서도 err를 그대로 넘기면 안 된다. url.Parse의 오류 텍스트에 전체 주소가 들어 있다.
		return nil, Permanent(fmt.Errorf("잘못된 요청 주소: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 연결 거부, DNS 실패, 시간 초과는 대개 일시적인 장애라 백오프 재시도에 맡긴다.
		//
		// 오류 텍스트는 민감 정보를 가린 뒤 밖으로 넘겨야 한다. 이유: http.Client.Do가 돌려주는 것은 *url.Error이고,
		// 그 Error()는 `Op "전체URL": 바탕 오류`인데, 이 기능의 자격 증명은 **URL 안에 있다**
		// (DingTalk access_token, WeCom key, Feishu hook id, Telegram /bot<token>/).
		// 가리지 않으면 자격 증명이 이 오류 문자열을 따라 네 곳으로 흘러간다. notification_deliveries의
		// last_error(평문 저장), 전달 이력 API 응답(**알림 채널 설정의 마스킹을 우회**),
		// 서버 로그, 테스트 발송 API가 프런트에 돌려주는 502 텍스트다.
		return nil, fmt.Errorf("요청 실패: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("응답 읽기 실패: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429(속도 제한)와 408(시간 초과)은 재시도할 만하다. 나머지 4xx는 설정이나 권한 문제라 재시도해도 소용없다.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("상대가 속도를 제한했거나 시간이 초과됐습니다(HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("상대 서비스 오류(HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("상대가 요청을 거부했습니다(HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet 은 응답 본문을 짧은 한 줄 텍스트로 줄여 오류 메시지에 쓴다. 응답에는 줄바꿈과 많은 공백이 있을 수 있어,
// 그대로 last_error에 넣으면 전달 이력 화면의 배치가 깨진다.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget 은 전달 주소를 'scheme://host/…'로 줄여 오류 메시지에 쓴다.
//
// 이 패키지에서 주소의 민감 정보를 가리는 유일한 기준이며, 일부러 **충분히 거칠게** 만들었다. scheme과 host 말고는
// 모두 버린다. URL의 어느 부분이 자격 증명인지 '범용적이고 안전하게' 판단할 방법이 없기 때문이다.
//
//	DingTalk 자격 증명이 query에       /robot/send?access_token=xxx
//	WeCom    자격 증명이 query에       /cgi-bin/webhook/send?key=xxx
//	Feishu   자격 증명이 **경로 끝**에  /open-apis/bot/v2/hook/<hook_id>
//	Telegram 자격 증명이 **경로 중간**에 /bot<token>/sendMessage
//
// '쓸모 있는 부분만 남기려면' 알림 채널마다 덧대야 하고, 하나라도 빠뜨리면 자격 증명이 한 번 유출된다.
// host만 남겨도 문제를 찾기에는 충분하다(DNS 조회 실패, 연결 안 됨, 인증서 오류 모두 찾을 수 있다).
// 어느 봇인지는 알림 채널 설정에 보이는 마스킹된 끝자리로 알아본다.
//
// 파싱에 실패하면 고정된 자리표시자를 돌려준다. 원래 문자열은 절대 돌려주지 않는다.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(파싱할 수 없는 주소)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError 는 전송 계층 오류에서 주소를 떼어 내고 바탕 원인만 남긴다.
//
// *url.Error의 구조는 {Op, URL, Err}이고 Error()가 URL까지 함께 출력한다.
// 여기서는 Err 필드를 명시적으로 꺼내 그 Error()를 피한다. 나중에 문자열을 바꾸는 것보다 믿을 만하다.
// 바꾸기는 URL 인코딩/이스케이프된 여러 변형을 제대로 다뤄야 해서 빠뜨리기 쉽다.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 알 수 없는 오류", uerr.Op, host)
	}
	// *url.Error가 아닌 오류(리다이렉트 정책이 돌려준 오류 등)에도 주소가 들어 있을 수 있어 똑같이 가린다.
	return redactURLsInText(err.Error())
}

// redactURLsInText 는 텍스트 안에 나오는 http(s) 주소를 가린 형태로 바꾼다.
//
// 구조화된 필드를 얻을 수 없는 오류(리다이렉트 정책 오류, 서드파티 라이브러리의 사용자 지정 오류)의 대비책으로 쓴다.
// http/https 접두사만 알아보고 공백과 따옴표로 나눈다. 주소에는 이 두 종류의 문자가 들어가지 않는다.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
