package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// refusalTestTokens 는 Claude 구독 transport가 붙일 고정 토큰을 준다. 실제 계정에 닿지 않는다.
type refusalTestTokens struct{}

func (refusalTestTokens) Token(context.Context) (string, string, error) {
	return "refusal-test-access-token", "", nil
}
func (refusalTestTokens) Invalidate(string) {}

// newClaudeRefusalServer 는 HTTP 200으로 명시적 거절(stop_reason=refusal)만 돌려주는 가짜 Claude 서버다.
func newClaudeRefusalServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"},\"usage\":{\"output_tokens\":0}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","content":[],"stop_reason":"refusal","usage":{"input_tokens":10,"output_tokens":0}}`)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newRefusalTestServer 는 New를 거치지 않은 최소 Server에 거절하는 Claude 구독 제공자를 전역 제공자로 단다.
// New를 쓰지 않으므로 다른 작업의 백그라운드 루프가 이 테스트와 겹치지 않는다.
func newRefusalTestServer(t *testing.T, m *Manager, baseURL string, stream bool) *Server {
	t.Helper()
	cfg := agent.ConfigFrom("anthropic", "claude-refusal-test", baseURL, "", "")
	cfg.AuthType = db.AuthClaudeOAuth
	cfg.OAuthTokens = refusalTestTokens{}
	cfg.Stream = stream
	prov, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := NewEngine(m)
	e.debounce = 0
	s := &Server{m: m, engine: e, ctx: ctx, taskAgents: map[string]*taskAgentBundle{},
		chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{},
		llmProv: prov, llmCfg: cfg, llmOn: true}
	t.Cleanup(func() {
		cancel()
		stopTaskRuntimes(e)
	})
	return s
}

func newRefusalTestTask(t *testing.T, m *Manager, timeoutSeconds int) *Task {
	t.Helper()
	task, err := m.CreateTask("거절 회귀 테스트", "https://refusal.example.test", nil, timeoutSeconds, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	return task
}

// assertRefusalFailedTask 는 거절이 작업을 failed로 끝내고, 그 사유를 활동 기록에 남겼는지 본다.
// 재시작 경로는 저장된 상태만 보고 종료 상태 작업을 다시 띄우지 않으므로 PG에 저장된 상태까지 확인한다.
func assertRefusalFailedTask(t *testing.T, m *Manager, task *Task) {
	t.Helper()
	if got := m.TaskStatus(task.ID); got != "failed" {
		t.Fatalf("거절 뒤 작업 상태가 failed가 아니다: %q", got)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	stored, err := m.pg.GetTask(id)
	if err != nil || stored == nil || stored.Status != "failed" {
		t.Fatalf("거절 뒤 저장된 작업 상태가 failed가 아니다: %+v err=%v", stored, err)
	}
	activities, _, err := task.Store.ActivityList(nil, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range activities {
		if a.Worker == "system" && strings.Contains(a.Summary, "거절") {
			return
		}
	}
	t.Fatalf("거절 사유가 활동 기록에 없다: %+v", activities)
}

func TestGoalDecompositionRefusalFailsTask(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{{"stream", true}, {"complete", false}} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewManager(t.TempDir(), "")
			if err != nil {
				t.Skipf("postgres unavailable (%v) - skipping", err)
			}
			t.Cleanup(func() { _ = m.Close() })
			var calls atomic.Int32
			ts := newClaudeRefusalServer(t, &calls)
			s := newRefusalTestServer(t, m, ts.URL, tc.stream)
			task := newRefusalTestTask(t, m, 3600)

			s.startTaskEngine(task)

			if calls.Load() != 1 {
				t.Fatalf("거절된 목표 분해 요청을 다시 보냈다: calls=%d", calls.Load())
			}
			assertRefusalFailedTask(t, m, task)
			if _, started := s.engine.started.Load(task.ID); started {
				t.Fatal("목표 분해가 거절됐는데 플래너·워커 루프를 띄웠다")
			}
			// 마감 조정자는 종료 상태 작업을 마무리하지 않아야 한다(마무리하면 최종 계획 회차로 LLM을 다시 부른다).
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.engine.deadlineCoordinator(ctx, task)
			if ctx.Err() != nil || calls.Load() != 1 {
				t.Fatalf("거절로 끝난 작업에 마감 처리가 LLM을 다시 불렀다: calls=%d ctxErr=%v", calls.Load(), ctx.Err())
			}
		})
	}
}

func TestPlannerRefusalFailsTaskWithoutFurtherCalls(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	var calls atomic.Int32
	ts := newClaudeRefusalServer(t, &calls)
	s := newRefusalTestServer(t, m, ts.URL, true)
	task := newRefusalTestTask(t, m, 0)
	if _, err := task.Store.AddNode(db.KindGoal, map[string]any{"text": "관리자 권한 확인"}, 0, "open", "system", nil); err != nil {
		t.Fatal(err)
	}
	// 해석기가 불릴 때마다 알린다. 한 회차의 해석기 호출을 받은 뒤 다음 깨우기의 해석기 호출을 받으면
	// 앞 회차가 끝났다는 뜻이다(plannerLoop는 회차를 한 goroutine에서 차례로 돈다).
	resolved := make(chan struct{})
	s.engine.SetAuthoritativeAgentResolver(func(rt *Task) (*agent.Planner, *agent.Worker) {
		if rt.ID != task.ID {
			return nil, nil
		}
		b := s.agentsForTask(rt)
		resolved <- struct{}{}
		return b.pl, b.wk
	})
	runLoop := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			s.engine.plannerLoop(ctx, task)
			close(done)
		}()
		return func() { cancel(); <-done }
	}
	wake := func() {
		task.notify <- struct{}{}
		select {
		case <-resolved:
		case <-time.After(10 * time.Second):
			t.Fatal("플래너 회차가 시작되지 않았다")
		}
	}

	stop := runLoop()
	wake() // 첫 회차: 플래너가 거절을 받는다
	wake() // 둘째 회차의 시작 = 첫 회차가 끝남
	if calls.Load() != 1 {
		t.Fatalf("거절된 계획 요청을 다시 보냈다: calls=%d", calls.Load())
	}
	assertRefusalFailedTask(t, m, task)
	stop()

	// 재시작으로 루프가 다시 떠도 종료 상태 작업은 계획하지 않는다.
	stop = runLoop()
	wake()
	wake()
	stop()
	if calls.Load() != 1 {
		t.Fatalf("거절로 끝난 작업을 다시 계획했다: calls=%d", calls.Load())
	}
}
