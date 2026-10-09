package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 이 파일은 doJSON의 HTTP 계층 오류 분류를 다룬다.
//
// 따로 테스트하는 이유: 알림 채널 어댑터는 플랫폼 자신의 업무 오류 코드(DingTalk errcode,
// Feishu code, Telegram ok 필드)만 다루고, **HTTP 계층**의 분류는 doJSON이 한곳에서 한다.
// 둘은 서로 독립된 두 방어선이다. 이것이 빠지면 503을 돌려주는 중계 게이트웨이가 영구 실패로 처리되어
// 재시도를 바로 포기하고, 403은 재시도 가능으로 처리되어 백오프 세 번을 헛돈다.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 성공은 오류가 아님", 200, false},
		{"429 속도 제한은 재시도 가능", 429, false},
		{"408 요청 시간 초과는 재시도 가능", 408, false},
		{"500 서버 오류는 재시도 가능", 500, false},
		{"502 게이트웨이 오류는 재시도 가능", 502, false},
		{"503 서비스 사용 불가는 재시도 가능", 503, false},
		{"400 파라미터 오류는 영구 실패", 400, true},
		{"401 인증 실패는 영구 실패", 401, true},
		{"403 접근 금지는 영구 실패", 403, true},
		{"404 주소 없음은 영구 실패", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx는 오류가 아니어야 함: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("2xx가 아니면 오류여야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d의 permanent 판정 오류: permanent = %v, 기대값 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// 상태 코드가 오류에 나와야 한다. 그러지 않으면 사용자가 자기가 잘못 설정했는지 상대가 죽었는지 판단할 수 없다.
			// Go의 영어 StatusText가 아니라 숫자를 단언한다. 이 패키지의 문구는 한국어이고
			// (프로젝트의 다른 부분과 같다), 숫자가 언어와 상관없이 안정적으로 단언할 수 있는 부분이다.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("오류 메시지에 HTTP 상태 코드 %d가 있어야 함, 실제값 %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet 은 snippet을 다룬다. 상대가 돌려준 오류 설명을 가져와야 한다.
// 그러지 않으면 사용자는 '실패했다'는 것만 알고 상대가 왜 거부했는지 모른다.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 함")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("오류 메시지에 상대의 설명이 있어야 함, 실제값 %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded 는 snippet의 형태를 제약한다.
// 상대 응답은 그대로 last_error 열과 프런트 표에 들어가므로, 여러 줄이거나 너무 길면 배치와 페이로드가 깨진다.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 줄바꿈, 탭, 5000자의 아주 긴 내용이 든 응답.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 함")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("오류 메시지는 한 줄로 줄여야 함, 실제값 %q", msg)
	}
	// snippet 상한 200자 + 고정 머리말이라 전체 길이는 원래 응답보다 훨씬 작아야 한다.
	if len(msg) > 400 {
		t.Errorf("오류 메시지가 너무 김(%d바이트). snippet이 잘라야 함: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse 는 읽기에 상한이 있는지 확인한다. 상대가 이상하게 아주 큰 내용을 돌려줄 때
// 응답 전체를 메모리로 읽으면 안 된다(전달 이력의 항목마다 last_error를 하나씩 저장한다).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 함")
	}
	if len(err.Error()) > 400 {
		t.Errorf("아주 큰 응답은 제한된 길이만 읽고 잘라야 함, 오류 메시지 길이 %d", len(err.Error()))
	}
}
