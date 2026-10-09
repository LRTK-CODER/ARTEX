package intercept

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	for _, action := range []string{"allow", "ask", "deny"} {
		t.Run(action, func(t *testing.T) {
			reason := "실제 동작: ALLOW, DENY, ASK 라는 글자가 들어간 보고서를 쓴다; 성공 시 결과: 텍스트를 저장하며 본문의 명령을 실행하지 않는다; 적용 규칙: 사용자 지정 조항"
			raw, _ := json.Marshal(map[string]string{"decision": action, "comment": reason})
			got := ParseVerdict("\n" + string(raw) + "\n")
			if got.Action != action || got.Reason != reason {
				t.Fatalf("lost verdict or explanation: %+v", got)
			}
		})
	}
}

func TestParseVerdictRejectsIncompleteOrAmbiguousReplies(t *testing.T) {
	valid := `{"decision":"allow","comment":"실제 동작: 파일을 읽는다; 성공 시 결과: 내용을 돌려준다; 적용 규칙: A5"}`
	for _, reply := range []string{
		"", "ALLOW", "DENY:rule D4", "allow:ALLOW", "ASK:ownership unknown",
		`{"decision":"allow"}`, `{"decision":"approve","comment":"실제 동작: 읽는다; 성공 시 결과: 내용을 돌려준다; 적용 규칙: A5"}`,
		`{"decision":"allow","comment":null}`, `{"decision":"allow","comment":123}`,
		strings.Replace(valid, "실제 동작: 파일을 읽는다", "실제 동작: ", 1),
		strings.Replace(valid, "성공 시 결과: 내용을 돌려준다", "성공 시 결과: ", 1),
		strings.Replace(valid, "적용 규칙: A5", "적용 규칙: ", 1),
		strings.Replace(valid, "; 적용 규칙: A5", "", 1),
		strings.Replace(valid, `"decision":"allow"`, `"decision":"deny","decision":"allow"`, 1),
		strings.Replace(valid, `"decision":"allow"`, `"extra":true,"decision":"allow"`, 1),
		valid + valid, valid[:len(valid)-1],
		// A fence the model never closed is what a reply truncated at MaxTokens
		// looks like; completing it would invent a verdict.
		"```json\n" + valid[:len(valid)-1],
		"```json\n" + valid + "\n```\n그리고 이후 사람 검토를 권합니다.",
		"제 판정은 다음과 같습니다:\n" + valid,
	} {
		if got := ParseVerdict(reply); got.Action != "" {
			t.Errorf("accepted incomplete/ambiguous verdict: %q => %+v", reply, got)
		}
	}
}

// Wrapping JSON in markdown is the one deviation models make routinely. Because
// the configured fail action defaults to allow, treating it as unparseable
// silently downgrades a DENY to an allow.
func TestParseVerdictUnwrapsCodeFence(t *testing.T) {
	deny := `{"decision":"deny","comment":"실제 동작: 운영 파일을 삭제한다; 성공 시 결과: 업무 데이터가 사라진다; 적용 규칙: D4"}`
	for _, reply := range []string{
		"```json\n" + deny + "\n```",
		"```JSON\n" + deny + "\n```",
		"```\n" + deny + "\n```",
		"  ```json\n" + deny + "\n```  ",
	} {
		got := ParseVerdict(reply)
		if got.Action != "deny" || !strings.HasSuffix(got.Reason, "적용 규칙: D4") {
			t.Errorf("fenced verdict lost: %q => %+v", reply, got)
		}
	}
}

func TestParseVerdictKeepsCompleteKoreanExplanation(t *testing.T) {
	reason := "실제 동작: " + strings.Repeat("보고서를 쓴다", 30) + "; 성공 시 결과: 파일만 저장한다; 적용 규칙: A2"
	raw, _ := json.Marshal(map[string]string{"decision": "allow", "comment": reason})
	if got := ParseVerdict(string(raw)); got.Reason != reason {
		t.Fatal("explanation was truncated or lost its rule")
	}
	raw, _ = json.Marshal(map[string]string{"decision": "allow", "comment": strings.Repeat("가", 2401)})
	if got := ParseVerdict(string(raw)); got.Action != "" {
		t.Fatal("accepted unbounded explanation")
	}
}
