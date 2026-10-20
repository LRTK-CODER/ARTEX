package server

import (
	"context"
	"errors"
	"log"
	"strconv"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// agentUIDUnavailableMessage 는 에이전트 uid를 받지 못했을 때 작업 활동·대화에 보이는 문장이다.
const agentUIDUnavailableMessage = "에이전트 명령을 돌릴 uid를 받지 못했습니다."

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

// reserveAgentUID 는 uid 분리를 켰을 때 작업이 에이전트 명령을 돌리기 전에 작업 uid를 받아 둔다(#166). 받지 못하면
// 작업 루프를 시작하지 않고 원인을 활동에 남긴 뒤 false를 돌려준다.
//
// 풀이 바닥났으면 기다리지 않고 작업을 failed로 끝낸다. 그 밖의 오류는 일시적이라 작업 상태를 바꾸지 않는다. 작업을
// 다시 열거나 재개하면(Engine.Run) 다시 받는다. 꺼져 있으면 늘 true다.
func (e *Engine) reserveAgentUID(ctx context.Context, t *Task) bool {
	if e.sandbox == nil {
		return true
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err == nil {
		_, err = e.sandbox.TaskUID(ctx, e.m.pg, taskID)
	}
	if err == nil {
		return true
	}
	if !errors.Is(err, db.ErrAgentUIDPoolExhausted) {
		log.Printf("[engine] task %s 에이전트 uid를 받지 못해 이번 실행을 멈춤(상태 유지, 다음 실행에서 다시 받음): %v", t.ID, err)
		e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true,
			Summary: agentUIDUnavailableMessage + " 이번 실행을 멈춥니다. 작업을 다시 열거나 재개하면 다시 받습니다",
			Detail:  err.Error()})
		return false
	}
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
	return false
}
