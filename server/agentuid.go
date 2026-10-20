package server

import (
	"context"
	"log"
	"strconv"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// agentUIDUnavailableMessage 는 에이전트 uid를 받지 못했을 때 작업 활동·대화에 보이는 문장이다.
const agentUIDUnavailableMessage = "에이전트 명령을 돌릴 uid를 받지 못했습니다."

// reserveAgentUID 는 uid 분리를 켰을 때 작업이 에이전트 명령을 돌리기 전에 작업 uid를 받아 둔다(#166).
// 받지 못하면(풀 고갈, DB 오류) 기다리지 않고 작업을 원인과 함께 failed로 끝내고 false를 돌려준다. uid 없이 돌면
// 2단계(#177)부터는 그 작업의 명령이 돌 uid가 없기 때문이다. 꺼져 있으면 늘 true다.
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
