package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// agentUIDUnavailableMessage 는 에이전트 uid를 받지 못했을 때 작업 활동·대화에 보이는 문장이다.
const agentUIDUnavailableMessage = "에이전트 명령을 돌릴 uid를 받지 못했습니다."

// 작업 uid를 일시 오류로 받지 못했을 때 다시 시도하는 간격이다. 처음 간격에서 시작해 두 배씩 늘리고 최대 간격에서 멈춘다.
// DB가 잠깐 끊긴 정도면 첫 시도에서 받고, 오래 끊겨도 1분에 한 번만 DB를 두드린다.
const (
	agentUIDRetryInitial = 5 * time.Second
	agentUIDRetryMax     = 60 * time.Second
)

// agentUIDBackoff 는 작업 uid를 다시 받는 간격과 시계다. 제로값이면 위 상수와 실제 시계를 쓴다. 테스트는 짧은 간격과
// 가짜 시계를 넣는다.
type agentUIDBackoff struct {
	initial, max time.Duration
	now          func() time.Time
	after        func(time.Duration) <-chan time.Time
}

func (b agentUIDBackoff) first() time.Duration {
	if b.initial > 0 {
		return b.initial
	}
	return agentUIDRetryInitial
}

func (b agentUIDBackoff) limit() time.Duration {
	if b.max > 0 {
		return b.max
	}
	return agentUIDRetryMax
}

func (b agentUIDBackoff) next(d time.Duration) time.Duration {
	return min(2*d, b.limit())
}

func (b agentUIDBackoff) clockNow() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b agentUIDBackoff) wait(d time.Duration) <-chan time.Time {
	if b.after != nil {
		return b.after(d)
	}
	return time.After(d)
}

// agentUIDRetry 는 작업 하나의 uid 재시도다. Engine.uidRetries에 작업마다 하나만 있다.
type agentUIDRetry struct {
	cancel context.CancelFunc
}

// agentUIDRetryHint 는 uid를 받지 못한 원인에 맞춰 사용자가 할 일을 알려 준다. 풀 고갈은 작업이나 대화를 지우기
// 전에는 풀리지 않는다. 그 밖의 오류(DB 연결이 잠깐 끊김, 서버 종료로 ctx 취소)는 다시 하면 받을 수 있다.
//
// 어느 경우든 uid 없이 실행하지 않는다(#177). 명령이 공용 uid나 서버 uid로 떨어져 다른 작업의 프로세스·자료에
// 닿을 수 있어서다.
func agentUIDRetryHint(err error) string {
	if errors.Is(err, db.ErrAgentUIDPoolExhausted) {
		return "쓰지 않는 작업이나 대화를 지운 뒤 다시 시도하세요"
	}
	return "잠시 뒤 다시 시도하세요"
}

// acquireTaskAgentUID 는 uid 분리를 켰을 때 작업 uid를 받는다(#166). 이미 받았으면 같은 값이라 여러 번 불러도 된다.
// 꺼져 있으면 늘 nil이다.
func (e *Engine) acquireTaskAgentUID(ctx context.Context, t *Task) error {
	if e.sandbox == nil {
		return nil
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("task id %q: %w", t.ID, err)
	}
	if e.taskUID != nil {
		_, err = e.taskUID(ctx, taskID)
	} else {
		_, err = e.sandbox.TaskUID(ctx, e.m.pg, taskID)
	}
	return err
}

// reserveAgentUID 는 작업이 에이전트 명령을 돌리기 전에 작업 uid를 받아 둔다. 받으면 true다. 받지 못하면 작업
// 루프를 시작하지 않는다. uid 없이 실행하지 않는다(#177).
//
//   - 풀 고갈: 작업이나 대화를 지우기 전에는 풀리지 않으므로 기다리지 않고 작업을 failed로 끝낸다.
//   - ctx 취소(서버 종료): 다시 시도할 곳이 없으므로 원인만 남긴다. 다음 서버 시작의 복원에서 다시 받는다.
//   - 그 밖의 오류(DB 연결이 잠깐 끊김 등): 작업 상태를 running으로 둔 채 그 작업의 실행 자리에서 물러나며 다시
//     받는다(retryAgentUID). 사용자가 다시 열거나 서버를 다시 시작하지 않아도 받으면 시작한다.
func (e *Engine) reserveAgentUID(ctx context.Context, t *Task) bool {
	err := e.acquireTaskAgentUID(ctx, t)
	switch {
	case err == nil:
		return true
	case errors.Is(err, db.ErrAgentUIDPoolExhausted):
		e.failTaskWithoutAgentUID(t, err)
	case ctx.Err() != nil:
		log.Printf("[engine] task %s 에이전트 uid를 받지 못해 이번 실행을 멈춤(서버 종료, 상태 유지): %v", t.ID, err)
		e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true,
			Summary: agentUIDUnavailableMessage + " 서버를 끝내는 중이라 이번 실행을 멈춥니다. 서버를 다시 시작하면 다시 받습니다",
			Detail:  err.Error()})
	default:
		e.retryAgentUID(ctx, t, err)
	}
	return false
}

// failTaskWithoutAgentUID 는 풀 고갈로 uid를 받지 못한 작업을 원인과 함께 failed로 끝낸다.
func (e *Engine) failTaskWithoutAgentUID(t *Task, err error) {
	won, setErr := e.m.SetTaskStatusGuarded(t.ID, "failed")
	switch {
	case setErr != nil:
		log.Printf("[engine] task %s 에이전트 uid를 받지 못했고 failed 기록도 실패: %v (%v)", t.ID, err, setErr)
	case won:
		log.Printf("[engine] task %s 에이전트 uid를 받지 못해 failed로 끝냄: %v", t.ID, err)
		e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true,
			Summary: agentUIDUnavailableMessage + " 작업을 실패로 끝냅니다. " + agentUIDRetryHint(err),
			Detail:  err.Error()})
		e.cancelExec(t.ID, agent.Causef("agent_uid_unavailable", "에이전트 uid를 받지 못해 작업이 실패로 끝남",
			"%v", err))
	default:
		log.Printf("[engine] task %s 에이전트 uid를 받지 못했으나 이미 종료 상태라 원래 상태를 유지함: %v", t.ID, err)
	}
}

// retryAgentUID 는 일시 오류로 uid를 받지 못한 작업의 실행 자리(작업 runtime)에 재시도 루틴 하나를 띄운다. 이미
// 다시 시도 중이면 아무것도 하지 않는다. 원인과 다음 시도 시각은 활동에 한 번만 남긴다(시도마다 쌓으면 DB가 오래
// 끊겼을 때 활동이 그것으로 가득 찬다).
//
// 루틴은 uid를 받으면 작업 루프를 평소처럼 시작한다. 작업 일시 중지·삭제(stopAgentUIDRetry), 작업 runtime 정지
// (StopTask), 서버 종료(ctx 취소)로 다음 시도를 기다리지 않고 끝난다. 일시 중지로 끝난 작업은 재개할 때 Run이
// 다시 받는다.
func (e *Engine) retryAgentUID(ctx context.Context, t *Task, cause error) {
	retryCtx, cancel := context.WithCancel(ctx)
	retry := &agentUIDRetry{cancel: cancel}
	if _, loaded := e.uidRetries.LoadOrStore(t.ID, retry); loaded {
		cancel()
		return
	}
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		e.uidRetries.CompareAndDelete(t.ID, retry)
		cancel()
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1)
	e.deleteMu.RUnlock()

	delay := e.uidBackoff.first()
	log.Printf("[engine] task %s 에이전트 uid를 받지 못해 %s 뒤 다시 받음(상태 유지): %v", t.ID, delay, cause)
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true,
		Summary: fmt.Sprintf("%s 일시적인 오류라 %s에 다시 시도합니다. 받을 때까지 최대 %s 간격으로 다시 시도하고, 받으면 작업을 시작합니다",
			agentUIDUnavailableMessage, e.uidBackoff.clockNow().Add(delay).Format("15:04:05"), e.uidBackoff.limit()),
		Detail: cause.Error()})

	runTaskRoutine(rt, func(loopCtx context.Context) {
		defer e.uidRetries.CompareAndDelete(t.ID, retry)
		defer cancel()
		stop := context.AfterFunc(loopCtx, cancel)
		defer stop()
		for {
			select {
			case <-retryCtx.Done():
				log.Printf("[engine] task %s 에이전트 uid 재시도를 멈춤(일시 중지·삭제·서버 종료)", t.ID)
				return
			case <-e.uidBackoff.wait(delay):
			}
			err := e.acquireTaskAgentUID(retryCtx, t)
			switch {
			case err == nil:
				log.Printf("[engine] task %s 에이전트 uid를 받아 작업을 시작함", t.ID)
				// 시작 전에 재시도 표시를 지운다. startTaskLoops가 Run처럼 started를 보고 중복 시작을 막는다.
				e.uidRetries.CompareAndDelete(t.ID, retry)
				e.startTaskLoops(ctx, t)
				return
			case errors.Is(err, db.ErrAgentUIDPoolExhausted):
				e.failTaskWithoutAgentUID(t, err)
				return
			case retryCtx.Err() != nil:
				log.Printf("[engine] task %s 에이전트 uid 재시도를 멈춤(일시 중지·삭제·서버 종료): %v", t.ID, err)
				return
			}
			delay = e.uidBackoff.next(delay)
			log.Printf("[engine] task %s 에이전트 uid를 아직 받지 못해 %s 뒤 다시 받음: %v", t.ID, delay, err)
		}
	})
}

// stopAgentUIDRetry 는 작업의 uid 재시도가 있으면 다음 시도를 기다리지 않고 끝낸다.
func (e *Engine) stopAgentUIDRetry(taskID string) {
	if v, ok := e.uidRetries.Load(taskID); ok {
		v.(*agentUIDRetry).cancel()
	}
}
