package server

import (
	"errors"
	"fmt"
	"log"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// failTaskOnModelRefusal 은 err가 제공자의 명시적 거절이면 작업을 failed로 끝내고 true를 돌려준다.
// stage는 사용자에게 보일 단계 이름(예: "목표 분해")이다.
//
// 거절은 같은 요청을 다시 보내도 결과가 달라지리라 볼 근거가 없다. 그대로 두면 목표 분해는 원래 목표로
// 대체되고 플래너는 빈 회차로 끝나, 작업이 아무것도 실행하지 않은 채 제한 시간까지 남는다. 그래서 바로
// 종료 상태로 만든다. 종료 상태 작업은 플래너 회차, 마감 마무리, 재시작 복원이 모두 건너뛰므로 이
// 작업에 대한 자동 LLM 호출이 더 일어나지 않는다. 실행 중인 워커는 취소해 종료 상태 분기에서 stopped로
// 분류되게 한다.
func (e *Engine) failTaskOnModelRefusal(t *Task, stage string, err error) bool {
	if !errors.Is(err, agent.ErrModelRefused) {
		return false
	}
	won, setErr := e.m.SetTaskStatusGuarded(t.ID, "failed")
	switch {
	case setErr != nil:
		log.Printf("[engine] task %s %s 요청이 거절됐으나 failed 기록 실패: %v", t.ID, stage, setErr)
	case won:
		log.Printf("[engine] task %s %s 요청을 LLM 제공자가 거절해 failed로 끝냄: %v", t.ID, stage, err)
		e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true,
			Summary: fmt.Sprintf("%s 요청을 LLM 제공자가 거절해 작업을 실패로 끝냅니다. 같은 요청을 다시 보내거나 다른 프로필로 넘기지 않습니다", stage),
			Detail:  err.Error()})
		e.cancelExec(t.ID, agent.Causef("model_refused", "LLM 제공자가 요청을 거절해 작업이 실패로 끝남",
			"%s 요청을 LLM 제공자가 명시적으로 거절해 작업을 failed로 바꾸고, 뒤이어 아직 실행 중인 워커를 취소했습니다", stage))
	default:
		log.Printf("[engine] task %s %s 요청이 거절됐으나 이미 종료 상태라 원래 상태를 유지함", t.ID, stage)
	}
	return true
}
