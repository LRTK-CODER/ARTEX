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

// agentUIDBlocksRun 은 uid를 받지 못한 오류가 실행을 막아야 하는지 가린다. 풀 고갈만 막는다(#166).
// 고갈은 작업이나 대화를 지우기 전에는 풀리지 않으니 기다리지 않고 원인을 보여 준다. 그 밖의 오류(DB 연결이
// 잠깐 끊김, 서버 종료로 ctx 취소)는 일시적이라 실행을 막지 않고 다음 실행에서 다시 받는다. 1단계에서는 uid를
// 실행에 쓰지 않으므로 이렇게 해야 동작이 #175와 같다.
//
// TODO: #177 - uid로 명령을 돌리기 시작하면 일시 오류 때 uid 없이 돌 수 없다. 그때 이 경우의 처리를 다시 정한다.
func agentUIDBlocksRun(err error) bool {
	return errors.Is(err, db.ErrAgentUIDPoolExhausted)
}

// reserveAgentUID 는 uid 분리를 켰을 때 작업이 에이전트 명령을 돌리기 전에 작업 uid를 받아 둔다(#166).
// 풀이 바닥났으면 기다리지 않고 작업을 원인과 함께 failed로 끝내고 false를 돌려준다. 그 밖의 오류는 로그만
// 남기고 true다(agentUIDBlocksRun). 꺼져 있으면 늘 true다.
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
	if !agentUIDBlocksRun(err) {
		log.Printf("[engine] task %s 에이전트 uid를 받지 못했으나 일시 오류로 보고 그대로 시작함(다음 실행에서 다시 받음): %v", t.ID, err)
		return true
	}
	won, setErr := e.m.SetTaskStatusGuarded(t.ID, "failed")
	switch {
	case setErr != nil:
		log.Printf("[engine] task %s 에이전트 uid를 받지 못했고 failed 기록도 실패: %v (%v)", t.ID, err, setErr)
	case won:
		log.Printf("[engine] task %s 에이전트 uid를 받지 못해 failed로 끝냄: %v", t.ID, err)
		e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true,
			Summary: agentUIDUnavailableMessage + " 작업을 실패로 끝냅니다. 쓰지 않는 작업이나 대화를 지운 뒤 다시 시작하세요",
			Detail:  err.Error()})
		e.cancelExec(t.ID, agent.Causef("agent_uid_unavailable", "에이전트 uid를 받지 못해 작업이 실패로 끝남",
			"%v", err))
	default:
		log.Printf("[engine] task %s 에이전트 uid를 받지 못했으나 이미 종료 상태라 원래 상태를 유지함: %v", t.ID, err)
	}
	return false
}
