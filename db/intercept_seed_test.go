package db

import (
	"regexp"
	"testing"
)

// 내장 "삭제 API 경로" 규칙은 tool_input JSON 문자열 전체와 맞춰 본다. 그래서 테스트
// 입력도 Interceptor가 실제로 받는 subject와 같은 JSON 형태로 준다.
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET으로 삭제 API 호출
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST로 삭제 API 호출
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // v1 경로 규칙은 접미사를 허용하지 않아 여기서 보충한다
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("일치해야 하는데 통과함: %s", s)
		}
	}

	// 동사 뒤에는 구분자가 있어야 한다. /delivery, /details 같은 읽기 전용 경로를 잘못 차단하지 않기 위해서다.
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // 삭제 동사가 경로가 아니라 도메인에 있다
		`{"command":"curl 'http://t.com/remote/status'"}`,
		`{"command":"nmap -p80 10.0.0.1"}`,
	}
	for _, s := range miss {
		if re.MatchString(s) {
			t.Errorf("잘못 차단함: %s", s)
		}
	}
}
