package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/jackc/pgx/v5/pgconn"
)

// isFKViolation reports whether err is a Postgres foreign-key violation (SQLSTATE
// 23503) — e.g. an activity insert whose exploration_id has no parent row.
func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// dropReason은 활동 기록이 버려진 이유를 분류해, 로그를 원본 오류 문구가 아니라
// 원인별로 묶어 볼 수 있게 한다.
func dropReason(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return "fk_violation(23503, 상위 탐색 없음)"
		case "23505":
			return "unique_violation(23505)"
		default:
			return "pg_error(" + pgErr.Code + ")"
		}
	}
	return "write_error"
}

// bumpDrop increments and returns the running count of dropped (unpersistable)
// activity records for a task. Concurrent planner + worker emits race here, so the
// counter is an atomic behind sync.Map. The count in the log shows loss scale at a
// glance instead of forcing a grep-and-count.
func (e *Engine) bumpDrop(taskID string) int64 {
	v, _ := e.dropCnt.LoadOrStore(taskID, new(int64))
	return atomic.AddInt64(v.(*int64), 1)
}

// preview collapses newlines and trims s to a short rune-safe snippet for one-line
// log output (avoids dumping a multi-KB summary/detail into the log).
func preview(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// model_error(제공자·API 장애: LLM 계층의 일시적 재시도를 다 썼거나, 스트림이 시작된 뒤 중간에
// 끊김)로 끝난 work 는 "해 봤지만 다 못 함"도 진짜 실패도 아니고 외부의 일시적 흔들림이다. 기본처럼
// 영구 blocked 로 두면 탐색 의도 하나를 그냥 잃으므로, 이 종료 상태는 몇 번 더 재실행한다. 재시도
// 사이에 백오프를 두어 제공자가 회복할 시간을 준다. 재시도 중 일시 중지·중지·취소되면 바로 해당
// 분기에 넘긴다.
const (
	modelErrorRetries      = 2               // model_error 로 끝난 뒤 더 재시도하는 횟수
	modelErrorRetryBackoff = 3 * time.Second // 재시도 전마다 두는 백오프
	workControlWaitTimeout = 30 * time.Second
)

var errWorkControlConflict = errors.New("work control conflict")

// retryableWorkerModelError 는 라우터가 처리한 오류와 Claude 구독의 재전송 금지 오류를 뺀다.
// 일부 출력 뒤 한도 오류로 프로필이 바뀌어도 전체 의도를 백업에서 다시 실행하지 않는다.
func retryableWorkerModelError(reason harness.TerminalReason, err error) bool {
	var claudeErr *agent.ClaudeRequestError
	return reason == harness.ReasonModelError && !isTaskLLMRuntimeError(err) && !errors.As(err, &claudeErr)
}

// Engine drives the event-driven exploration loop with real LLM agents
// (docs §4.3/§4.4): on asset/exploration-graph change (debounced) it wakes the
// planner, which reads the route, queries assets, judges goals and emits intents;
// N concurrent work agents claim intents and execute them. There is no
// simulation mode — an LLM provider is required. The planner/worker can be
// (re)installed at runtime (LLM configured from the UI); the loops always run
// but idle until an LLM is set.
type Engine struct {
	m        *Manager
	debounce time.Duration

	bc *Broadcaster // live activity pub/sub (SSE)

	started  sync.Map // taskID -> bool, so Run is idempotent per task
	lastAct  sync.Map // taskID -> int64 unix, last planner/worker activity (heartbeat)
	llmCalls sync.Map // taskID -> *int64, actual planner/worker/main-agent LLM calls
	paused   sync.Map // taskID -> bool, user-paused (planner + workers idle but loops alive)
	deleting sync.Map // taskID -> bool, delete barrier (no new task-owned writes)
	dropCnt  sync.Map // taskID -> *int64, running count of dropped (unpersistable) activity records

	// deleteMu makes installing the delete barrier atomic with registering a new
	// task operation. Once BeginDelete returns, every admitted writer is reflected
	// in inflight and every later writer is rejected.
	deleteMu sync.RWMutex

	// Every long-lived task goroutine (planner, workers and deadline coordinator)
	// runs under one task-scoped context. Successful deletion cancels that context,
	// waits for all goroutines, then releases every task-level Engine reference.
	runtimeMu sync.Mutex
	runtimes  map[string]*taskRuntime

	// per-task execution context: each planner.Plan / worker.Execute runs under it,
	// so pausing can CANCEL an in-flight run (not just skip the next one). Recreated
	// on resume since cancelling is one-shot. Every cancellation carries a named
	// cause so the activity trace can identify the initiating control path.
	execMu     sync.Mutex
	execCancel map[string]context.CancelCauseFunc
	execCtx    map[string]context.Context

	// Per-work control lets the planner kill a worker and lets the UI pause/cancel
	// one intent without pausing the whole task. The done channel closes only after
	// runWorkerStep has stopped writing and committed its final state.
	workMu sync.Mutex
	work   map[int64]*workExecution

	// steerBox queues planner course-corrections for a running work (keyed by intent
	// id). The worker's PreToolUse hook drains it before its next tool call and hands
	// the message to the model (blocking that call) so it re-plans — no kill needed.
	steerMu  sync.Mutex
	steerBox map[int64][]string

	plannerRound sync.Map // taskID -> int, planner round counter (for UI round separators)

	// 작업 단위 시간 초과(docs/任务级超时与收尾设计.md 참고):
	settling     sync.Map // taskID -> bool, 작업이 마무리 단계에 들어갔다(새 탐색 의도를 나눠 주거나 할당받지 않는다)
	deadline     sync.Map // taskID -> int64 unix, 절대 마감 시각(처음 실행할 때 기록한다. 0이나 없음 = 제한 없음)
	stamped      sync.Map // taskID -> bool, first_run_at 을 기록했는지(이 프로세스에서 한 번만 기록한다)
	inflight     sync.Map // taskID -> *int64, 실행 중인 planner.Plan + worker.Execute 수(drain 에 쓴다)
	coordStarted sync.Map // taskID -> bool, deadline 조정자를 시작했는지(Run/reload 중복 방지)

	// resolve returns a task's dedicated planner/worker (wired by the server as the
	// authoritative task-router). nil,nil means this task is deliberately unavailable
	// (for example an exhausted failover chain) — there is no global-pair fallback.
	resolve              func(t *Task) (*agent.Planner, *agent.Worker)
	resolveAuthoritative bool
	// readiness reports whether a global LLM provider is configured — the signal behind
	// Ready()/the llm_configured indicator. Wired once at startup; nil → not ready.
	readiness func() bool
}

type taskRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type workExecution struct {
	cancel context.CancelCauseFunc
	done   chan error
	action string // user action: pause | cancel
}

// nextPlannerRound returns the next planner round number for a task (1-based).
func (e *Engine) nextPlannerRound(taskID string) int {
	v, _ := e.plannerRound.LoadOrStore(taskID, 0)
	n := v.(int) + 1
	e.plannerRound.Store(taskID, n)
	return n
}

// Pause stops a task: marks it paused AND cancels any in-flight planner/worker run
// for it (a long worker.Execute would otherwise keep going until it finishes).
func (e *Engine) Pause(taskID string, cause error) {
	e.paused.Store(taskID, true)
	e.cancelExec(taskID, cause)
}

// BeginDelete installs an execution barrier before task data/files are removed.
// The temporary pause is not a user pause. The server serializes this transition
// with lifecycle admission and tells AbortDelete whether the persisted task is
// paused/queued if cleanup fails.
func (e *Engine) BeginDelete(taskID string) bool {
	e.deleteMu.Lock()
	if _, loaded := e.deleting.LoadOrStore(taskID, true); loaded {
		e.deleteMu.Unlock()
		return false
	}
	e.paused.Store(taskID, true)
	e.deleteMu.Unlock()
	e.cancelExec(taskID, agent.AbortTaskDeleted)
	return true
}

func (e *Engine) AbortDelete(taskID string, keepPaused bool) {
	e.deleteMu.Lock()
	if !e.IsDeleting(taskID) {
		e.deleteMu.Unlock()
		return
	}
	e.deleting.Delete(taskID)
	if !keepPaused {
		e.paused.Delete(taskID)
	}
	e.deleteMu.Unlock()
	if !keepPaused && e.m != nil {
		if t, ok := e.m.Task(taskID); ok {
			t.Notify()
		}
	}
}

func (e *Engine) IsDeleting(taskID string) bool {
	_, ok := e.deleting.Load(taskID)
	return ok
}

// registerTaskRoutines reserves count goroutines in the task runtime. Callers
// hold deleteMu for reading so StopTask cannot race WaitGroup.Add with Wait.
func (e *Engine) registerTaskRoutines(parent context.Context, taskID string, count int) *taskRuntime {
	e.runtimeMu.Lock()
	defer e.runtimeMu.Unlock()
	rt := e.runtimes[taskID]
	if rt == nil {
		ctx, cancel := context.WithCancel(parent)
		rt = &taskRuntime{ctx: ctx, cancel: cancel}
		e.runtimes[taskID] = rt
	}
	rt.wg.Add(count)
	return rt
}

func runTaskRoutine(rt *taskRuntime, fn func(context.Context)) {
	go func() {
		defer rt.wg.Done()
		fn(rt.ctx)
	}()
}

// StopTask permanently stops every long-lived goroutine and removes all Engine
// state for a successfully deleted task. The delete barrier remains installed
// until cleanup finishes, so no new task operation can race the teardown.
func (e *Engine) StopTask(taskID string) {
	e.deleteMu.Lock()
	e.deleting.Store(taskID, true)
	e.deleteMu.Unlock()

	e.cancelExec(taskID, agent.AbortTaskDeleted)
	e.runtimeMu.Lock()
	rt := e.runtimes[taskID]
	if rt != nil {
		rt.cancel()
	}
	e.runtimeMu.Unlock()
	if rt != nil {
		rt.wg.Wait()
	}

	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(agent.AbortTaskDeleted)
	}
	delete(e.execCancel, taskID)
	delete(e.execCtx, taskID)
	e.execMu.Unlock()

	e.runtimeMu.Lock()
	if e.runtimes[taskID] == rt {
		delete(e.runtimes, taskID)
	}
	e.runtimeMu.Unlock()

	e.started.Delete(taskID)
	e.lastAct.Delete(taskID)
	e.llmCalls.Delete(taskID)
	e.paused.Delete(taskID)
	e.dropCnt.Delete(taskID)
	e.plannerRound.Delete(taskID)
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.inflight.Delete(taskID)
	e.coordStarted.Delete(taskID)
	e.deleteMu.Lock()
	e.deleting.Delete(taskID)
	e.deleteMu.Unlock()
}

// cancelExec cancels a task's current per-task exec context (any in-flight
// planner.Plan / worker.Execute), if present. Shared by Pause and the settle
// sequence's hard-drain backstop.
func (e *Engine) cancelExec(taskID string, cause error) {
	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(cause)
	}
	e.execMu.Unlock()
}

// Resume un-pauses a task and nudges a fresh planning round. The next exec under
// it gets a fresh (uncancelled) context.
func (e *Engine) Resume(t *Task) {
	// BeginDelete owns the pause barrier once deletion starts. A concurrent
	// resume must never clear it and let a planner/worker re-enter while cleanup
	// is waiting for task operations to drain.
	if t == nil {
		return
	}
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(t.ID) {
		return
	}
	e.paused.Delete(t.ID)
	t.Notify()
}

// execContextFor returns a live per-task context derived from parent, recreating
// it if a prior pause cancelled it.
func (e *Engine) execContextFor(parent context.Context, taskID string) context.Context {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	if e.IsPaused(taskID) {
		// never hand out a live context while paused (guards the claim→Execute race)
		c, cancel := context.WithCancelCause(parent)
		cancel(agent.AbortPausedRaceGuard)
		return c
	}
	if c := e.execCtx[taskID]; c != nil && c.Err() == nil {
		return c
	}
	c, cancel := context.WithCancelCause(parent)
	e.execCtx[taskID] = c
	e.execCancel[taskID] = cancel
	return c
}

// IsPaused reports whether a task is user-paused.
func (e *Engine) IsPaused(taskID string) bool {
	v, ok := e.paused.Load(taskID)
	return ok && v.(bool)
}

// Started reports whether the engine loops are running for a task.
func (e *Engine) Started(taskID string) bool {
	_, ok := e.started.Load(taskID)
	return ok
}

// LastActivity returns the unix time of the last planner/worker activity for a
// task (0 if none yet).
func (e *Engine) LastActivity(taskID string) int64 {
	if v, ok := e.lastAct.Load(taskID); ok {
		return v.(int64)
	}
	return 0
}

// BeginLLMCall/EndLLMCall track actual provider calls separately from the
// scheduler's task-operation counter. A task can have live loops while all of
// them are waiting for a trigger; that state must remain idle in the UI.
func (e *Engine) BeginLLMCall(taskID string) {
	v, _ := e.llmCalls.LoadOrStore(taskID, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

func (e *Engine) EndLLMCall(taskID string) {
	if v, ok := e.llmCalls.Load(taskID); ok {
		p := v.(*int64)
		if atomic.AddInt64(p, -1) <= 0 {
			atomic.StoreInt64(p, 0)
		}
	}
}

func (e *Engine) ActiveLLMCalls(taskID string) int64 {
	if v, ok := e.llmCalls.Load(taskID); ok {
		return atomic.LoadInt64(v.(*int64))
	}
	return 0
}

func (e *Engine) touch(taskID string) { e.lastAct.Store(taskID, time.Now().Unix()) }

func NewEngine(m *Manager) *Engine {
	return &Engine{m: m, debounce: 800 * time.Millisecond, bc: NewBroadcaster(),
		execCancel: map[string]context.CancelCauseFunc{}, execCtx: map[string]context.Context{},
		work: map[int64]*workExecution{}, steerBox: map[int64][]string{},
		runtimes: map[string]*taskRuntime{}}
}

// registerWork records the cancel for the work currently running intentID.
func (e *Engine) registerWork(intentID int64, cancel context.CancelCauseFunc) {
	e.workMu.Lock()
	e.work[intentID] = &workExecution{cancel: cancel, done: make(chan error, 1)}
	e.workMu.Unlock()
}

// detachWork removes the live control handle once Execute has returned. complete
// must be called after the final intent state write so a waiting cancel handler can
// safely delete the worker's blackboard output without racing a late write.
func (e *Engine) detachWork(intentID int64) (action string, complete func(error)) {
	e.workMu.Lock()
	run := e.work[intentID]
	if run != nil {
		delete(e.work, intentID)
		action = run.action
		run.cancel(agent.AbortWorkFinished) // release resources (no-op if already cancelled)
	}
	e.workMu.Unlock()
	e.steerMu.Lock()
	delete(e.steerBox, intentID) // drop any undelivered steering for a finished work
	e.steerMu.Unlock()
	if run == nil {
		return action, func(error) {}
	}
	return action, func(err error) { run.done <- err }
}

// ControlWork requests a user-visible pause or cancellation and waits until the
// worker has fully stopped writing. Cancellation cleanup is performed by the API
// handler after this returns; pause state is committed by runWorkerStep itself.
func (e *Engine) ControlWork(ctx context.Context, intentID int64, action string) error {
	if action != "pause" && action != "cancel" {
		return fmt.Errorf("unsupported work action %q", action)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.workMu.Lock()
	run := e.work[intentID]
	if run == nil {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 탐색 의도 %d에서 실행 중인 work가 없습니다(이미 끝났거나 할당되지 않았습니다)", errWorkControlConflict, intentID)
	}
	if run.action != "" {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 탐색 의도 %d에서 %s 동작을 실행하는 중입니다", errWorkControlConflict, intentID, run.action)
	}
	run.action = action
	done := run.done
	cause := error(agent.AbortWorkPausedByUser)
	if action == "cancel" {
		cause = agent.AbortWorkCancelledByUser
	}
	run.cancel(cause)
	e.workMu.Unlock()

	timer := time.NewTimer(workControlWaitTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("탐색 의도 %d %s 마무리 대기: %w", intentID, action, ctx.Err())
	case <-timer.C:
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("탐색 의도 %d %s 마무리 대기: %w", intentID, action, context.DeadlineExceeded)
	}
}

// releaseWorkControl drops only this caller's reservation after its wait is
// cancelled. The work context stays cancelled; runWorkerStep recognizes the
// named cancellation cause and settles the intent into the recoverable paused
// state even if the HTTP caller has gone away.
func (e *Engine) releaseWorkControl(intentID int64, run *workExecution, action string) {
	e.workMu.Lock()
	if current := e.work[intentID]; current == run && current.action == action {
		current.action = ""
	}
	e.workMu.Unlock()
}

func transitionIntentState(store *db.ExplorationStore, intentID int64, expected, state string) error {
	changed, err := store.CompareAndSetIntentState(intentID, expected, state)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 탐색 의도 %d이(가) 더는 %s 상태가 아닙니다", db.ErrIntentStateConflict, intentID, expected)
	}
	return nil
}

// SteerWork queues a mid-run course-correction for the work running intentID (the
// planner's steer_work tool). The worker delivers it before its next tool call and
// re-plans — no kill. Errors if no work is currently running that intent.
func (e *Engine) SteerWork(intentID int64, msg string) error {
	if strings.TrimSpace(msg) == "" {
		return fmt.Errorf("steer message must not be empty")
	}
	e.workMu.Lock()
	running := e.work[intentID] != nil
	e.workMu.Unlock()
	if !running {
		return fmt.Errorf("no work is running for intent %d (it may have finished or not been claimed)", intentID)
	}
	e.steerMu.Lock()
	e.steerBox[intentID] = append(e.steerBox[intentID], msg)
	e.steerMu.Unlock()
	return nil
}

// drainSteer pops the oldest queued steering message for intentID (FIFO), if any.
func (e *Engine) drainSteer(intentID int64) (string, bool) {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	q := e.steerBox[intentID]
	if len(q) == 0 {
		return "", false
	}
	msg := q[0]
	if len(q) == 1 {
		delete(e.steerBox, intentID)
	} else {
		e.steerBox[intentID] = q[1:]
	}
	return msg, true
}

// steerHooks는 가드의 hook 실행기를 감싸 플래너가 실행 중인 작업의 방향을 틀 수 있게 한다.
// 도구 호출마다 대기 중인 방향 수정 메시지가 있으면 꺼내 그 호출을 막고 메시지를 모델에
// 돌려준다. 모델은 도구를 실행하는 대신 다음 단계를 다시 계획한다. 대기 메시지가 없으면
// 가드는 전과 똑같이 동작한다.
// 'thinking-only 회차'(공회전)의 이어 가기도 맡는다. Stop 참고.
type steerHooks struct {
	inner harness.HookRunner
	drain func() (string, bool)
	// nudges 는 이 탐색 의도에 넣은 공회전 이어 가기 횟수이고 최대는 limit 이다. 포인터인 이유:
	// harness 는 steerHooks 의 값 복사본을 들고 있으므로 횟수는 한 곳을 함께 써야 한다.
	nudges *atomic.Int64
	// limit 은 공회전 이어 가기의 최대 횟수로, Engine.emptyTurnNudgeLimit() 가 '빈 응답 재시도
	// 횟수'에서 구한다. <=0 이면 개입하지 않는다(사용자가 이 단계를 명시적으로 껐다).
	limit int
	// label 은 "worker-1 · #42" 같은 형태이고 로그에만 쓴다.
	label string
}

// 공회전 회차(사고만 있고 본문도 도구 호출도 없음)의 이어 가기 횟수 기본값이다. SDK 빈 응답 재시도의
// 내장 기본값(norma/llm/openai.go 의 emptyResponseRetries)과 맞춘다. 두 단계가 같은 설정 하나를
// 함께 쓰므로 설정하지 않았을 때의 동작도 같아야 한다. 값을 구하는 곳은 Engine.emptyTurnNudgeLimit.
//
// 이 수는 '연속 몇 번'이 아니라 '탐색 의도 하나의 총량'이다. harness 의 stopHookActive 가 이미
// 연속 공회전에는 한 번만 재촉하게 막는다. 재촉한 회차도 공회전이면 Stop 훅이 다시 불리지 않고 run 이
// 끝난다. 도구 회차가 한 번 실제로 일어나야 횟수가 새로 채워진다(norma/harness/query.go:534).
// 그래서 이 제한은 '도구 → 공회전 → 재촉 → 도구 → 공회전' 같은 비정상 반복이 탐색 의도의 단계 한도를
// 다 써 버리지 않게 막는다.
const defaultEmptyTurnNudges = 2

// emptyTurnNudge 는 공회전 회차에 넣는 이어 가기 지시다.
//
// harness 는 이런 회차를 자연스러운 종료로 본다(stop_reason=end_turn 이고 tool_use 없음). 그래서 다섯
// 단계의 LLM 재시도 중 어느 것도 걸리지 않는다. 오류가 아니라 모델이 "생각은 끝냈지만 손을 대지 않은"
// 것이기 때문이다. SDK 의 빈 응답 재시도도 닿지 않는다. 그 단계는 "이벤트를 yield 했는가"로 빈 응답을
// 가리는데, 사고 증분도 이벤트라서(norma/llm/openai.go 의 SEThinkingDelta) thinking-only 는 빈 응답이
// 아니다. 게다가 그 단계는 프롬프트 전체를 그대로 다시 보내므로, 컨텍스트 모양 때문에 생긴 공회전이면
// 모델이 같은 생각을 한 번 더 할 뿐이다. 그래서 여기서는 지시를 하나 덧붙여 이미 만든 사고를 이어
// 가게 한다. 입력이 바뀌어야 다른 행동을 기대할 수 있다.
const emptyTurnNudge = "[Idle turn notice] In your last turn you only produced reasoning: no reply text and no tool call, " +
	"so the turn produced nothing. Carry out the next step you just planned now: either call a tool or write your conclusion. Do not repeat the reasoning."

// isThinkingOnlyTurn reports whether the latest assistant turn produced neither
// text nor a tool call — i.e. the model spent the whole round thinking.
func isThinkingOnlyTurn(messages []llm.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		return strings.TrimSpace(m.Text()) == "" && len(m.ToolUses()) == 0
	}
	return false
}

func (h steerHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if msg, ok := h.drain(); ok {
		return true, "[Live course correction from the planner] " + msg +
			"\n(This is the planner's immediate instruction for this intent. This tool call was NOT executed; adjust your next step accordingly. If it conflicts with your current plan, follow this instruction.)", nil
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func (h steerHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

// Stop 은 guard 의 원래 동작 위에 공회전 회차 이어 가기를 더한다. 모델이 사고만 내고 본문도 도구
// 호출도 없으면 harness 는 이를 자연 종료로 보고 빈 summary 로 끝낸다(query.go 의
// ReasonCompleted + asst.Text()). 그러면 아직 끝나지 않은 탐색 의도가 중간에 끊긴다. 이때 이어 가기
// 지시를 넣어 모델이 이미 한 사고를 가지고 계속하게 한다.
func (h steerHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	var (
		prevent  bool
		blocking []string
		msg      string
	)
	if h.inner != nil {
		prevent, blocking, msg = h.inner.Stop(ctx, messages)
	}
	// inner 가 이미 강제로 멈추기로 했거나 자기 이어 가기 메시지를 넣으려 하면 그대로 따르고 겹치지 않는다.
	// limit<=0 은 사용자가 '빈 응답 재시도 횟수'를 -1로 설정해 이 단계를 명시적으로 끈 것이다.
	if prevent || len(blocking) > 0 || h.nudges == nil || h.limit <= 0 || !isThinkingOnlyTurn(messages) {
		return prevent, blocking, msg
	}
	n := h.nudges.Add(1)
	if n > int64(h.limit) {
		log.Printf("[work %s] 공회전 회차(사고만, 본문·도구 없음) 이어 가기 최대 %d회 도달, 종료 허용", h.label, h.limit)
		return prevent, blocking, msg
	}
	log.Printf("[work %s] 공회전 회차(사고만, 본문·도구 없음), 이어 가기 지시 주입 (%d/%d)", h.label, n, h.limit)
	return false, []string{emptyTurnNudge}, ""
}

// KillWork cancels the in-flight work running intentID (planner's kill_work tool).
// The work's agent-core session honors ctx cancellation and aborts promptly.
func (e *Engine) KillWork(intentID int64) error {
	e.workMu.Lock()
	run := e.work[intentID]
	e.workMu.Unlock()
	if run == nil {
		return fmt.Errorf("no work is running for intent %d (it may have finished or not been claimed)", intentID)
	}
	run.cancel(agent.AbortKilledByPlanner)
	return nil
}

// Broadcaster exposes the engine's live activity pub/sub (used by the SSE handler).
func (e *Engine) Broadcaster() *Broadcaster { return e.bc }

// emitActivity persists one captured step AND fans it out to live subscribers,
// from a single point so storage and the SSE stream never diverge.
func (e *Engine) emitActivity(t *Task, r db.Activity) db.Activity {
	id, err := e.appendActivity(t, r)
	if err != nil {
		// 더는 조용히 넘기지 않는다. 기록 하나를 버리면 활동 기록에서 명령↔결과 짝이 깨진다. tool_result 를
		// 잃은 tool_use 는 계속 '실행 중'으로 보이고, 'result'/'round' 기록을 잃으면 세션에 요약이 없다.
		// 근본 원인을 분석하는 데 필요한 것을 오류 수준 한 줄에 모두 담는다: 이유 분류, summary 미리
		// 보기, 이 작업에서 버린 누적 건수, 그리고 FK 경우에는 부모 exploration 에 닿지 못하는 이유를
		// 실제 DB에서 확인한 결과.
		n := e.bumpDrop(t.ID)
		diag := ""
		// On the FK-parent failure (23503) probe the live DB so the log records WHY the
		// exploration is unreachable (row gone / wrong expID) instead of just that it is.
		if isFKViolation(err) {
			storeID := t.Store.ID()
			if exists, refs, maxID, dErr := e.m.pg.ExplorationDiag(storeID); dErr != nil {
				diag = fmt.Sprintf(" | FK 진단 조회 실패(store.expID=%d task.ExpID=%d): %v", storeID, t.ExpID, dErr)
			} else {
				diag = fmt.Sprintf(" | FK 진단: store.expID=%d task.ExpID=%d exploration 있음=%v 참조하는 task 수=%d MAX(exploration.id)=%d",
					storeID, t.ExpID, exists, refs, maxID)
			}
		}
		log.Printf("[activity] task %s 활동 기록 버림(이 작업 누적 %d건째) worker=%s kind=%s tool=%s tuid=%s reason=%s summary=%q: %v%s",
			t.ID, n, r.Worker, r.Kind, r.Tool, r.ToolUseID, dropReason(err), preview(r.Summary, 80), err, diag)
		e.touch(t.ID)
		return r
	}
	r.ID = id
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	e.bc.Publish(t.ID, r)
	e.touch(t.ID)
	return r
}

// appendActivity persists one activity row, retrying briefly on write failure.
// Concurrent planner + worker inserts into the same exploration's activity log
// occasionally fail; a couple of quick retries recover most. Crucially, every
// failure is now LOGGED (it used to be swallowed by an `if err == nil`), so the
// underlying DB error is finally visible for diagnosis.
func (e *Engine) appendActivity(t *Task, r db.Activity) (int64, error) {
	var id int64
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if id, err = t.Store.AppendActivity(r); err == nil {
			if attempt > 1 {
				log.Printf("[activity] task %s 쓰기 재시도 %d회째 성공 (worker=%s kind=%s tool=%s)",
					t.ID, attempt, r.Worker, r.Kind, r.Tool)
			}
			return id, nil
		}
		log.Printf("[activity] task %s 쓰기 실패 (%d/3회, worker=%s kind=%s tool=%s expID=%d): %v",
			t.ID, attempt, r.Worker, r.Kind, r.Tool, t.Store.ID(), err)
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	return 0, err
}

// SetReadiness wires the global "an LLM provider is configured" predicate (read by
// Ready() / the llm_configured indicator). Called once at startup.
func (e *Engine) SetReadiness(fn func() bool) { e.readiness = fn }

// SetAgentResolver installs a per-task planner/worker resolver (wired by the server).
// Called once at startup before any task loop runs, so no lock is needed on reads.
func (e *Engine) SetAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = false
}

// SetAuthoritativeAgentResolver installs a resolver whose nil result must not
// fall through to the global provider. Task-level failover chains use this so a
// fully exhausted chain cannot silently bypass its configured boundary.
func (e *Engine) SetAuthoritativeAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = true
}

// snapshotFor returns the planner/worker a task should run on, from the task-router
// resolver. nil,nil means the task is deliberately unavailable (e.g. an exhausted
// failover chain); there is no global-pair fallback.
func (e *Engine) snapshotFor(t *Task) (*agent.Planner, *agent.Worker) {
	if e.resolve != nil {
		p, w := e.resolve(t)
		if (p != nil && w != nil) || e.resolveAuthoritative {
			return p, w
		}
	}
	return nil, nil
}

// Ready reports whether a global LLM provider is configured (via the readiness
// predicate wired at startup).
func (e *Engine) Ready() bool {
	return e.readiness != nil && e.readiness()
}

// ReadyFor reports whether a specific task can resolve a planner/worker pair.
// An explicit task profile chain can be runnable even when no global default
// provider is configured, so task status must not rely on Ready alone.
func (e *Engine) ReadyFor(t *Task) bool {
	p, w := e.snapshotFor(t)
	return p != nil && w != nil
}

// Run starts the planner loop + N worker loops for a task. The loops always run
// but no-op until an LLM is configured (so a task created while idle picks up
// automatically once LLM is set from the UI).
func (e *Engine) Run(ctx context.Context, t *Task) {
	workers := e.m.Workers()
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.started.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		t.Notify() // already running — just nudge a planning round
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1+workers)
	e.deleteMu.RUnlock()
	e.touch(t.ID)
	runTaskRoutine(rt, func(loopCtx context.Context) { e.plannerLoop(loopCtx, t) })
	for i := 0; i < workers; i++ {
		name := fmt.Sprintf("work#%d", i+1)
		runTaskRoutine(rt, func(loopCtx context.Context) { e.workerLoop(loopCtx, t, name) })
	}
	e.startDeadlineCoordinator(ctx, t) // 작업 단위 시간 초과 타이머(timeout>0 일 때만, 중복 방지)
	// 활성 탐색 의도(open+running)가 하나도 없을 때만 첫 계획 회차를 시작한다. 시드 탐색 의도가 있는
	// 작업은 시드가 open 이거나, 위에서 막 띄운 워커가 먼저 할당받아 running 이 됐다. 둘 다 "할 일이
	// 있음"이므로 첫 플래너 회차를 건너뛰고, 워커가 시드 탐색 의도를 바로 할당받아 실행한다. 끝나면
	// NotifyDone·하트비트가 플래너를 깨운다.
	// 주의: Frontier(open만 센다)를 쓰면 안 된다. 워커의 할당(open→running)과 이 검사가 경쟁해 잘못
	// 시작할 수 있다. 재시작 뒤 자동 복구할 때도 running 탐색 의도만 남을 수 있으니 마찬가지로 건너뛴다.
	if has, _ := t.Store.HasActiveIntent(); !has {
		t.Notify() // kick the first planning round (acted on once LLM is ready)
	}
}

// plannerHeartbeatInterval 은 작업의 플래너 하트비트 간격을 구한다. db.CreateTask 가 이미 정규화했지만
// (600 미만은 600으로 올린다) 메모리 상태의 이상한 값에 대비해 여기서 한 번 더 하한을 둔다.
func plannerHeartbeatInterval(t *Task) time.Duration {
	sec := t.PlanHeartbeatSeconds
	if sec < db.MinPlanHeartbeatSeconds { // 하한 = 기본값 = 600(10분)
		sec = db.MinPlanHeartbeatSeconds
	}
	return time.Duration(sec) * time.Second
}

// resetPlannerTimer 는 이미 울렸을 수 있는 Timer 를 안전하게 다시 건다(표준 Stop→drain→Reset 패턴).
func resetPlannerTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (e *Engine) plannerLoop(ctx context.Context, t *Task) {
	interval := plannerHeartbeatInterval(t)
	// 하트비트 타이머를 루프 입구에서 건다 = 작업 시작부터 잰다. 첫 플래너 회차를 건너뛴 시드 작업이라
	// 여기서 계속 막혀 있어도(Run 에서 frontier 가 비어 있지 않으면 첫 회차를 시작하지 않는다)
	// '작업 시작 + interval'에 하트비트가 첫 계획 회차를 시작한다. 그 뒤로는 깨어날 때마다(변경 신호·
	// 하트비트) 다시 건다 = 마지막 계획 시작부터 잰 시간.
	heartbeat := time.NewTimer(interval)
	defer heartbeat.Stop()

	// runRound 는 계획 회차 하나를 실행한다(debounce 병합 + 각 guard 포함). src 는 로그에서 트리거 출처를 가리는 데만 쓴다.
	runRound := func(src string) {
		// debounce: 잇따른 변경을 planner 한 라운드로 묶는다
		timer := time.NewTimer(e.debounce)
	drain:
		for {
			select {
			case <-ctx.Done():
				// 작업을 멈추거나 지울 때 StopTask가 이 루프의 반환을 기다리므로
				// debounce를 다 채우지 않고 바로 끝낸다(#81).
				timer.Stop()
				return
			case <-t.notify:
			case <-timer.C:
				break drain
			}
		}
		planner, _ := e.snapshotFor(t)
		if planner == nil {
			return // idle until LLM configured
		}
		if e.IsPaused(t.ID) {
			return // user-paused: don't plan
		}
		if e.IsDeleting(t.ID) {
			return
		}
		// terminal task (goals all met → done, or failed): the run is over. A
		// resume/nudge — e.g. auto-resume of the active task on restart — must NOT
		// re-plan (it would burn an LLM round and re-confirm a settled result).
		if isTerminalStatus(t.lifecycleSnapshot().Status) {
			return
		}
		// 작업 단위 시간 초과로 마무리하는 중: 일반 깨우기는 버린다. 워커의 마무리 기록이나 Resume 의
		// Notify 도 일반 계획 회차를 시작하지 않는다. 최종 회차는 조정자(settleTask)가 직접 돌리고 여기를 지나지 않는다.
		if e.isSettling(t.ID) {
			return
		}
		// goalless(사람이 직접 넣음) 분기: 작업에 open 목표가 없으면 플래너를 돌리지 않는다. 돌리면 met 를
		// 다시 판정해 cancelExec 로 사용자가 메인 에이전트를 거쳐 직접 넣은 탐색 의도를 죽인다. 끝낼지는
		// frontier 가 정한다. open/running 탐색 의도가 남았으면 running 으로 두고 조용히 기다리고, 모두
		// 끝났으면 done 으로 기록한다. 이 구간은 순수 Go 이고 LLM 호출도 계획 회차 표시도 하지 않는다.
		if open, err := t.Store.HasOpenGoal(); err == nil && !open {
			t.drainTriggers() // 쌓인 done/finding 트리거를 버려 긴 goalless 세션에서 끝없이 늘지 않게 한다
			if active, err := t.Store.HasActiveIntent(); err == nil && !active {
				// frontier 가 비고 실행 중인 탐색 의도도 없음 → 마무리. Guarded 버전으로 CAS 해서 동시에 일어나는
				// pause/delete/시간 초과 마무리의 상태 전환을 덮어쓰지 않게 한다.
				if won, err := e.m.SetTaskStatusGuarded(t.ID, "done"); err != nil {
					log.Printf("[goalless] task %s 마무리 done 기록 실패: %v", t.ID, err)
				} else if won {
					e.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
						Summary: "목표를 모두 달성했고 직접 넣은 탐색 의도도 모두 실행해 작업을 마칩니다"})
				}
			}
			return // goalless 분기는 planner.Plan 에 들어가지 않는다
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		defer e.decInflight(t.ID)
		e.stampFirstRun(t) // 처음 실제로 계획할 때 first_run_at 을 기록하고 deadline 을 계산한다(timeout 이 있는 작업만)
		e.touch(t.ID)
		emit := func(r db.Activity) { e.emitActivity(t, r) }
		ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // Pause 로 취소할 수 있고 작업 deadline 을 따른다
		if ectx.Err() != nil || e.IsDeleting(t.ID) {
			return
		}
		log.Printf("[planner] task %s 계획 중…(%s 트리거)", t.ID, src)
		// round marker: each Plan() is one planner round; emit a boundary so the
		// UI can separate rounds in the transcript (kind='round').
		e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
			Summary: fmt.Sprintf("%d번째 계획", e.nextPlannerRound(t.ID))})
		// what fired this round (worker done / finding; may be several — debounce
		// coalesces a burst; empty for time/heartbeat wakes).
		triggers := t.drainTriggers()
		taskIDInt, _ := strconv.ParseInt(t.ID, 10, 64)
		e.BeginLLMCall(t.ID)
		met, reason, err := planner.Plan(ectx, taskIDInt, e.m.assets, t.Store, t.Goal, triggers, emit)
		e.EndLLMCall(t.ID)
		switch {
		case err != nil && ectx.Err() == nil:
			log.Printf("[planner] task %s 계획 오류: %v", t.ID, err)
		case met:
			log.Printf("[planner] task %s 목표 달성 판정: %s", t.ID, reason)
			// 목표를 모두 달성함 → 작업 상태를 done 으로 저장한다(프런트엔드 DTO 가 이 종료 상태를 먼저 보여 준다).
			if err := e.m.SetTaskStatus(t.ID, "done"); err != nil {
				log.Printf("[planner] task %s 완료 상태 저장 실패: %v", t.ID, err)
			}
			// 작업을 완료로 판정함 → 실행 중인 워커를 바로 취소한다. 손에 든 탐색 의도의 결과가 더는 의미 없다.
			// 다음 워커 루프는 종료 상태 검사에 걸려 새 탐색 의도를 할당받지 않는다. 취소된 것들은 아래
			// '작업 완료' 분기에서 blocked 가 아니라 stopped 로 분류된다.
			e.cancelExec(t.ID, agent.AbortGoalMet)
		default:
			log.Printf("[planner] task %s 계획 완료", t.ID)
		}
		e.touch(t.ID)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.notify:
			runRound("edge") // 워커 종료 / finding / kill / resume / 시드 첫 회차
		case <-heartbeat.C:
			// 주기적 안전장치: 교착 대비 + 실행 중인 워커 감독(steer/kill)을 위한 깨우기 + 주기적 재검토.
			runRound("heartbeat")
		}
		// 깨어날 때마다(변경 신호나 하트비트) 하트비트를 다시 건다. 어떤 계획 시작이든 대기 시간을 새로 잰다.
		resetPlannerTimer(heartbeat, interval)
	}
}

func (e *Engine) workerLoop(ctx context.Context, t *Task, name string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, worker := e.snapshotFor(t)
		if worker == nil {
			if sleepCtx(ctx, 1500*time.Millisecond) {
				return
			}
			continue
		}
		if e.IsPaused(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // user-paused: don't claim/execute intents
		}
		if e.IsDeleting(t.ID) {
			return
		}
		if e.isSettling(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 작업 시간 초과로 마무리하는 중: 새 탐색 의도를 할당받지 않는다(실행 중인 것은 스스로 마무리하고 조정자가 drain 을 기다린다)
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 작업이 종료 상태다(done/failed/timeout): 남은 탐색 의도를 할당받지 않고, 완료 뒤 frontier 를 헛돌지 않는다
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		claimed := e.runWorkerStep(ctx, t, name, worker)
		e.decInflight(t.ID)
		if !claimed && sleepCtx(ctx, 800*time.Millisecond) {
			return
		}
	}
}

// runWorkerStep claims one intent from the frontier and fully settles it via
// runIntent. Returns false when nothing was claimable. The pool worker loop is its
// only caller.
func (e *Engine) runWorkerStep(ctx context.Context, t *Task, name string, worker *agent.Worker) bool {
	intent := e.claimNext(t, name)
	if intent == nil {
		return false
	}
	log.Printf("[worker %s] task %s 탐색 의도 #%d 할당받음", name, t.ID, intent.ID)
	return e.runIntent(ctx, t, name, worker, intent, "", "")
}

// runIntent executes and fully settles one already-claimed (state=running) intent.
// Both the pool worker loop (via runWorkerStep) and the human-message handler (via
// runDetachedIntent, a dedicated goroutine outside the worker pool) call it, so the
// execute/retry/state-write logic lives in exactly one place. A non-empty message
// is injected as this turn's input through ExecuteWithMessage; requestID keys the
// transcript marker that dedups re-injection across model_error retries. The caller
// must already hold one task-operation admission for the whole sequence so a delete
// cannot observe quiescence between the LLM return and the final DB writes.
func (e *Engine) runIntent(ctx context.Context, t *Task, name string, worker *agent.Worker, intent *db.Node, requestID, message string) bool {
	hasChatMessage := message != ""
	e.stampFirstRun(t) // 처음 실제로 실행할 때 first_run_at 을 기록하고 deadline 을 계산한다(timeout 이 있는 작업만)
	e.touch(t.ID)
	emit := func(r db.Activity) { e.emitActivity(t, r) }
	ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // Pause 로 취소할 수 있고 작업 deadline 을 따른다
	if ectx.Err() != nil || e.IsDeleting(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 탐색 의도 #%d 할당 뒤 되돌리기 실패: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// per-work child context so the planner's kill_work can stop just this work.
	workCtx, workCancel := context.WithCancelCause(ectx)
	e.registerWork(intent.ID, workCancel)
	// wrap the guard hooks so steer_work can inject a mid-run course-correction
	// for THIS intent (drained before the worker's next tool call).
	iid := intent.ID
	taskEmit := func(a db.Activity) {
		nid := iid
		a.NodeID, a.Worker = &nid, name
		emit(a)
	}
	label := fmt.Sprintf("%s · #%d", name, iid)
	workCtx = intercept.WithTaskContext(workCtx, t.ID, label, taskEmit)
	// nudges 는 일부러 model_error 재실행 루프 밖에 만든다. 공회전 이어 가기의 최대값은 '이 탐색
	// 의도'의 총량이라, 한 번 재실행했다고 횟수를 처음부터 다시 세면 안 된다.
	hooks := steerHooks{
		inner:  t.Guard.Hooks(),
		drain:  func() (string, bool) { return e.drainSteer(iid) },
		nudges: &atomic.Int64{},
		limit:  e.emptyTurnNudgeLimit(),
		label:  label,
	}
	wTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
	e.BeginLLMCall(t.ID)
	var reason harness.TerminalReason
	var wrote agent.WriteCounts
	var err error
	if hasChatMessage {
		reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
	} else {
		reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
	}
	e.EndLLMCall(t.ID)
	// model_error 로 끝남 → 몇 번 더 재실행한다(백오프 뒤 재시도). 탐색 의도가 아직 이 work 의 것이고,
	// 작업이 일시 중지·중지·취소되지 않았고 **마무리 단계도 아닐 때만** 재시도한다. 아니면 해당 분기에
	// 넘긴다(마무리 중에는 재시도하지 않아 백오프가 다른 워커의 정상 마무리 시간을 빼앗지 않게 한다).
	maxRetries, retryBackoff := e.modelErrorRetryPolicy()
	for attempt := 1; attempt <= maxRetries &&
		retryableWorkerModelError(reason, err) &&
		workCtx.Err() == nil && ectx.Err() == nil && !e.IsPaused(t.ID) && !e.isSettling(t.ID); attempt++ {
		log.Printf("[worker %s] task %s 탐색 의도 #%d model_error 로 끝남, %v 뒤 재시도 (%d/%d)",
			name, t.ID, intent.ID, retryBackoff, attempt, maxRetries)
		if sleepCtx(workCtx, retryBackoff) {
			break // 백오프 중에 취소됨(중지·일시 중지) → 아래 분기에 넘긴다
		}
		e.BeginLLMCall(t.ID)
		if hasChatMessage {
			reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
		} else {
			reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
		}
		e.EndLLMCall(t.ID)
	}
	// Capture kill state before detachWork cancels workCtx. kill = this work's
	// ctx was cancelled (planner kill_work) while the TASK ctx kept running; a
	// pause cancels the task ctx (ectx) instead. Checking workCtx.Err() AFTER
	// unregister would always be true (unregister cancels it) → every completed
	// work would be wrongly marked stopped.
	workCause := context.Cause(workCtx)
	killed := workCtx.Err() != nil && ectx.Err() == nil
	action, completeWork := e.detachWork(intent.ID)
	// A caller may stop waiting and release its in-memory reservation before the
	// agent honors cancellation. The named context cause remains authoritative and
	// still settles the stopped run into a recoverable state.
	if action == "" {
		switch {
		case errors.Is(workCause, agent.AbortWorkPausedByUser):
			action = "pause"
		case errors.Is(workCause, agent.AbortWorkCancelledByUser):
			action = "cancel"
		}
	}
	var controlErr error
	defer func() { completeWork(controlErr) }()
	if action == "pause" {
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 탐색 의도 #%d 일시 중지 상태 저장 실패: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 탐색 의도 #%d 일시 중지됨", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	if action == "cancel" {
		// Park the stopped run in paused before handing cleanup to the API. If the
		// request disconnects after cancellation, the intent remains recoverable and
		// a later cancel can finish cleanup instead of leaving a phantom running row.
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 탐색 의도 #%d 취소 표시 저장 실패: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 탐색 의도 #%d 중지됨, 취소 정리 대기", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// if a pause cancelled this run mid-flight, return the intent to the frontier
	// so it is re-claimed on resume — the worker will resume the prior LLM
	// conversation from its transcript instead of restarting from scratch.
	if ectx.Err() != nil && taskExecutionPaused(context.Cause(ectx)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 탐색 의도 #%d 작업 일시 중지 되돌리기 실패: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// 작업 시간 초과 마무리의 강제 cancel(pause 도 kill 도 아님)이 이 run 을 취소함 → exhausted(마무리됨)로
	// 분류하고 blocked 로 잘못 표시하지 않는다. 이때 워커는 보통 settlement 단계에서 결과를 이미 기록했다.
	if ectx.Err() != nil && e.isSettling(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "exhausted"); err != nil {
			log.Printf("[worker %s] task %s 탐색 의도 #%d 시간 초과 마무리 상태 저장 실패: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 탐색 의도 #%d 작업 시간 초과 마무리로 끝남(exhausted), 기록 %s", name, t.ID, intent.ID, wrote)
		e.touch(t.ID)
		return true
	}
	// 작업을 완료로 판정함(일반 경로의 done) → 위의 cancelExec 가 이 run 을 취소했다. 탐색 의도 결과는
	// 의미가 없으므로 stopped(blocked 가 아님)로 표시해 완료된 작업의 탐색 의도 상태를 어지럽히지 않는다.
	if ectx.Err() != nil && isTerminalStatus(e.m.TaskStatus(t.ID)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 탐색 의도 #%d 종료 상태 stopped 저장 실패: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 탐색 의도 #%d 작업 완료로 취소됨(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// killed by the planner: mark stopped (don't write back results, don't auto-reclaim).
	if killed {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 탐색 의도 #%d 플래너 중지 저장 실패: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 탐색 의도 #%d 중지됨(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		t.Notify()
		return true
	}
	if err != nil {
		log.Printf("[worker %s] intent %d: %v", name, intent.ID, err)
	}
	// 종료 분류: 단계 한도에 닿음 ≠ 완료. max_turns→exhausted(플래너가 이 방향을 시도했지만 다 끝내지
	// 못해 다른 각도가 필요함을 알게 한다. 다룬 것으로 보고 영구히 건너뛰지 않는다). 오류→blocked. 정상→done.
	state := "done"
	switch {
	case err != nil:
		state = "blocked"
	case reason == harness.ReasonMaxTurns:
		state = "exhausted"
		log.Printf("[worker %s] intent %d 단계 한도 도달(exhausted), 이번 기록 %s", name, intent.ID, wrote)
	case reason == harness.ReasonTimeout:
		state = "exhausted"
		log.Printf("[worker %s] intent %d 실행 시간 초과(exhausted), 마무리 뒤 기록 %s", name, intent.ID, wrote)
	}
	if state == "blocked" && isTaskLLMChainExhausted(err) {
		_ = t.Store.SetIntentBlockedReason(intent.ID, db.IntentBlockedLLMQuota)
	} else {
		if stateErr := transitionIntentState(t.Store, intent.ID, "running", state); stateErr != nil {
			log.Printf("[worker %s] task %s 탐색 의도 #%d 종료 상태 %s 저장 실패: %v", name, t.ID, intent.ID, state, stateErr)
		}
	}
	log.Printf("[worker %s] task %s 탐색 의도 #%d 종료: %s (기록 %s)", name, t.ID, intent.ID, state, wrote)
	e.touch(t.ID)
	t.NotifyDone(intent.ID) // results changed the graph -> wake the planner (with the just-finished intent id)
	return true
}

// runDetachedIntent runs one paused intent OUTSIDE the worker pool in its own
// goroutine — the human-message path. It transitions the intent paused->running
// itself (never through 'open'), so the pool, which only claims 'open', can never
// race it; the "at most one run per intent" invariant still holds because winning
// the CAS is the sole entry and work[intentID] was cleared when the pause settled.
// Because it does not compete for a frontier slot, a user message continues the
// worker immediately even when all pool slots are busy (mirroring how the
// main-agent chat handler starts its run directly). The spawned goroutine owns one
// task-operation admission for the whole run and roots its context at ctx (pass the
// server root, never the HTTP request, so a disconnect cannot strand the run while
// task pause/delete/shutdown still stops it). Returns an error if the run could not
// be started; the intent is left untouched in that case.
func (e *Engine) runDetachedIntent(ctx context.Context, t *Task, intentID int64, requestID, message, agentMessage string) error {
	if !e.beginTaskOperation(t.ID) {
		return fmt.Errorf("task is being deleted")
	}
	release := true
	defer func() {
		if release {
			e.decInflight(t.ID)
		}
	}()
	_, worker := e.snapshotFor(t)
	if worker == nil {
		return fmt.Errorf("워커가 아직 준비되지 않았습니다")
	}
	node, err := t.Store.GetNode(intentID)
	if err != nil {
		return err
	}
	if node == nil || node.Kind != db.KindIntent {
		return fmt.Errorf("intent not found")
	}
	changed, err := t.Store.CompareAndSetIntentState(intentID, "paused", "running")
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 탐색 의도가 더는 paused 상태가 아닙니다", db.ErrIntentStateConflict)
	}
	node.State, node.Owner = "running", "chat"
	// Record the human turn as a visible activity BEFORE the run starts, so it is
	// ordered ahead of any worker step and never appears without the run happening.
	// Keep the UI copy concise; ExecuteWithMessage writes the server-resolved
	// reference snapshot into the intent transcript as the LLM input.
	uid := intentID
	e.emitActivity(t, db.Activity{NodeID: &uid, Worker: "user", Kind: "user", Summary: message, Detail: message})
	release = false // ownership of the admission passes to the goroutine
	go func() {
		defer e.decInflight(t.ID)
		e.runIntent(ctx, t, "chat", worker, node, requestID, agentMessage)
	}()
	return nil
}

func taskExecutionPaused(cause error) bool {
	var abort *agent.AbortCause
	if !errors.As(cause, &abort) {
		return false
	}
	switch abort.Code {
	case "paused_by_user", "paused_by_orchestrator", "paused_on_reload", "paused_race_guard",
		"queued_for_admission", "llm_unavailable_queued", "task_deleted":
		return true
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) (done bool) {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func (e *Engine) claimNext(t *Task, name string) *db.Node {
	fr, _ := t.Store.Frontier(20)
	for _, in := range fr {
		if ok, _ := t.Store.ClaimIntent(in.ID, name); ok {
			return in
		}
	}
	return nil
}
