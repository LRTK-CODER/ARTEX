package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "사용자가 작업을 일시 중지함",
		"사용자가 작업 제어 API(POST /api/tasks/{id}/control, action=pause)로 작업을 일시 중지했습니다. 이번 플래너/워커 실행은 취소됐습니다. 실행 중이던 의도는 frontier(open)로 되돌아가고, 작업을 재개하면 다시 할당받아 처음부터 실행합니다")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "오케스트레이션 에이전트가 작업을 일시 중지함",
		"오케스트레이션 에이전트가 pause_task 도구를 호출해 이 작업을 일시 중지했습니다. 이번 플래너/워커 실행은 취소됐습니다. 실행 중이던 의도는 frontier(open)로 되돌아가고, 재개하면 다시 실행합니다")
	AbortTaskDeleted = cause("task_deleted", "작업이 삭제됨",
		"작업을 삭제하는 중입니다(DELETE /api/tasks/{id}). 삭제 배리어가 이 작업에서 실행 중이던 플래너, 워커, 메인 에이전트를 취소했습니다. 이번 실행 결과는 더 이상 쓰이지 않습니다")
	AbortPausedOnReload = cause("paused_on_reload", "백엔드가 작업의 일시 중지 상태를 복원함",
		"백엔드가 시작할 때 데이터베이스에 영속된 상태에 따라 작업 일시 중지를 복원했습니다. 이번 실행은 취소됐습니다. 정상적인 경우 복원 단계에는 실행 중인 에이전트가 없습니다")
	AbortGoalMet = cause("goal_met", "플래너가 작업 목표 달성으로 판정함",
		"플래너가 작업 목표를 달성했다고 판정해 작업을 done으로 바꾸고, 뒤이어 아직 실행 중인 워커를 취소했습니다. 이 의도들은 실패가 아니라 stopped로 표시됩니다")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "작업 시간 초과 마무리의 대기 시간이 모두 소진됨",
		"작업이 timeout에 이른 뒤 실행 중인 워커가 정상 마무리하기를 기다렸지만 90초 drain 유예 시간으로도 부족해 강제 취소를 실행했습니다. 의도는 exhausted로 표시되고, 마무리 단계에서 이미 기록한 사실과 자산은 보존됩니다")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "플래너가 이 의도를 종료함",
		"플래너가 kill_work를 호출해 이 의도를 직접 종료했습니다. 보통 방향이 어긋났거나 더 진행할 가치가 없다는 뜻입니다. 의도는 stopped로 표시되고 자동으로 다시 할당받지 않습니다")
	AbortWorkPausedByUser = cause("work_paused_by_user", "사용자가 이 워커 의도를 일시 중지함",
		"사용자가 실행 중인 워커를 일시 중지했습니다. 이번 호출은 취소되고 의도는 paused로 바뀝니다. 이미 등록된 의도, 사실, 취약점, 활동 기록은 모두 보존되며, 재개하면 처음부터 다시 실행합니다")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "사용자가 이 워커 의도를 삭제함",
		"사용자가 실행 중인 워커를 삭제했습니다. 이번 호출은 취소됐습니다. 워커가 쓰기 구간을 벗어난 뒤, 서버는 사용자가 고른 삭제 방식에 따라 이 의도를 처리합니다. 소프트 삭제는 삭제됨으로만 표시하고 모든 결과물을 보존하며, 영구 삭제는 이 의도와 이 의도만이 떠받치는 하위 노드를 연쇄 삭제합니다")
	AbortWorkFinished = cause("work_finished", "워커가 정상 종료하고 context를 해제함",
		"워커가 정상적으로 끝나, 엔진이 detachWork 에서 그 context 자원을 해제했습니다. 이것은 실행 중단이 아닙니다. 중단 메시지에 나타난다면 취소와 종료 이벤트 사이에 경쟁 상태가 생겼다는 뜻입니다")
	AbortPausedRaceGuard = cause("paused_race_guard", "작업 일시 중지 동안 새 실행 시작을 거부함",
		"작업이 일시 중지 상태일 때 엔진이 새 실행 context 발급을 거부합니다. 할당받기와 일시 중지 사이의 경쟁 상태로 워커가 계속 시작되는 것을 막기 위해서입니다. 이미 할당받은 의도는 frontier로 되돌아갑니다")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "사용자가 이번 대화를 중지함",
		"사용자가 중지를 눌러 이번 메인 에이전트 또는 세션 에이전트 실행을 중단했습니다. 이미 생성된 활동 기록은 보존되며, 다음 메시지를 이어서 보낼 수 있습니다")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "작업 일시 중지로 메인 에이전트 대화가 중단됨",
		"사용자가 작업을 일시 중지할 때 실행 중이던 메인 에이전트 대화도 함께 취소됐습니다. 이미 생성된 활동 기록은 보존되며, 작업을 재개해도 이번 메시지를 자동으로 다시 재생하지 않습니다")
	AbortChatTurnFinished = cause("chat_turn_finished", "이번 대화가 정상 종료하고 context를 해제함",
		"이번 대화가 정상적으로 끝나, 서버가 그 대화의 context 자원을 해제하는 중입니다. 이것은 실행 중단이 아닙니다. 중단 메시지에 나타난다면 취소와 종료 이벤트 사이에 경쟁 상태가 생겼다는 뜻입니다")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "백엔드 프로세스가 종료 중임",
		"백엔드 프로세스가 SIGINT 또는 SIGTERM을 받아 재시작, 업데이트, 종료 중입니다. 실행 중인 모든 에이전트가 취소됩니다. 재시작 뒤 남아 있는 running 의도는 open으로 초기화돼 다시 실행됩니다")
	AbortRunHardTimeout = cause("run_hard_timeout", "한 번 실행의 강제 시간 제한에 걸림",
		"한 번 실행이 실제 경과 시간 예산과 추가 유예 시간을 넘겼습니다. 모델 요청이나 어떤 도구가 오래 반환하지 않아 정상적인 회차 경계 마무리를 수행하지 못했다는 뜻입니다. 중단 직전에 반환하지 않은 마지막 도구 호출을 중점적으로 확인하세요")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "상위 context가 deadline에 도달함",
			"상위 context가 deadline에 도달했지만, 설정한 쪽이 WithTimeoutCause로 이름 있는 원인을 붙이지 않았습니다: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "취소한 쪽이 이름 있는 원인을 붙이지 않음",
			"상위 context가 취소됐지만, 취소한 쪽이 context.WithCancelCause로 이름 있는 원인을 붙이지 않았습니다. agent/cancelcause.go에 원인을 등록하고 그 취소 지점에 연결하세요", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
