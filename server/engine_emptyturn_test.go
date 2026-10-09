package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 공회전 회차(사고만, 본문·도구 없음)를 알아보고 이어 가게 하는지 확인한다. steerHooks.Stop 참고.

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "먼저 포트를 스캔한다"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"사고만", []llm.Message{llm.UserText("시작"), assistantThinking("생각한다")}, true},
		{"사고+도구", []llm.Message{llm.UserText("시작"), toolUse}, false},
		{"사고+본문", []llm.Message{assistantThinking("생각한다"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("결론")},
		}}, false},
		{"본문이 공백 문자뿐", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"완전히 빈 assistant 회차", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// 도구 결과는 user 역할이므로, 가장 가까운 메시지로 잘못 판정하지 말고 그 앞의 assistant 까지 거슬러 봐야 한다.
		{"마지막이 도구 결과", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant 메시지 없음", []llm.Message{llm.UserText("시작")}, false},
		{"빈 기록", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks 는 값을 정할 수 있는 inner HookRunner 로, steerHooks 가 inner 의 결정을 따르는지 확인한다.
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("먼저 하위 도메인을 열거해야 한다")}

	t.Run("공회전 회차에 이어 가기 지시를 넣는다", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("공회전 회차에서 강제로 멈추면 안 됨")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("본문이나 도구가 있으면 개입하지 않는다", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("스캔을 마쳤고 열린 포트를 찾지 못했다")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("정상 종료를 공회전으로 잘못 판정함: blocking = %v, 기대값 nil", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("개입하지 않았을 때 nudges = %d, 기대값 0", n)
		}
	})

	t.Run("최대 횟수에 닿으면 종료를 허용한다", func(t *testing.T) {
		const limit = 5 // 사용자가 '빈 응답 재시도 횟수'를 5로 설정함
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d번째: 아직 한도 안이어야 함, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("최대 횟수를 넘었는데도 주입함: blocking = %v, 기대값 nil", blocking)
		}
	})

	// '빈 응답 재시도 횟수'를 -1로 설정하면 이 단계가 꺼지고 emptyTurnNudgeLimit 는 0이 된다.
	t.Run("설정으로 끄면 개입하지 않는다", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("껐는데도 주입함: blocking = %v, 기대값 nil", blocking)
		}
	})

	t.Run("inner 가 강제로 멈추면 겹치지 않는다", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard 가 종료를 거부함"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard 가 종료를 거부함" || blocking != nil {
			t.Fatalf("inner 의 강제 멈춤이 바뀜: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner 에 넘길 때 nudges = %d, 기대값 0", n)
		}
	})

	t.Run("inner 가 이미 이어 가게 하면 겹치지 않는다", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard 의 이어 가기 이유"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard 의 이어 가기 이유" {
			t.Fatalf("inner 의 이어 가기 메시지가 바뀜: blocking = %v", blocking)
		}
	})

	t.Run("횟수 카운터가 없으면 동작이 그대로다", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 예: 앞으로 다른 호출 지점이 nudges 를 넘기지 않은 경우
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("카운터 없이 주입함: blocking = %v, 기대값 nil", blocking)
		}
	})
}
