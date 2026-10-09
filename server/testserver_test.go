package server

import (
	"context"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// newTestServer 는 New로 서버를 띄우고, 테스트가 끝날 때 서버가 띄운 백그라운드 작업을
// 멈추고 끝나기를 기다린다. New는 DB에 남은 미완료 작업의 planner·worker 루프를
// 복원하는데, 기다리지 않으면 그 루프가 t.TempDir 정리와 다음 테스트의 New(전역 훅
// 재설정)와 겹쳐 TempDir 정리 실패와 DATA RACE가 난다.
// t.Cleanup은 등록 역순으로 돌므로 이 정리는 먼저 만든 t.TempDir의 삭제보다 앞선다.
// 호출자는 m을 닫는 정리를 이 함수보다 먼저 t.Cleanup으로 등록해, 루프가 멈춘 뒤에
// DB가 닫히게 한다(defer m.Close()는 이 정리보다 먼저 돈다).
func newTestServer(t *testing.T, m *Manager, skillDir, dataDir, keyDir string) *Server {
	t.Helper()
	restoreHooks := saveAgentHooks()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, m, skillDir, dataDir, keyDir)
	t.Cleanup(func() {
		cancel()
		stopTaskRuntimes(s.engine)
		s.archiveWG.Wait()
		<-s.side.done
		// New가 agent 전역 훅에 이 테스트의 DB를 묶는다. 되돌리지 않으면 New를 거치지 않고
		// Server를 만드는 다음 테스트가 그 훅으로 이미 닫힌 DB를 읽는다.
		restoreHooks()
	})
	return s
}

// saveAgentHooks 는 New가 바꾸는 agent 전역 훅을 저장하고, 되돌리는 함수를 돌려준다.
func saveAgentHooks() (restore func()) {
	toolAugment, toolResolve, trafficBinding := agent.ToolAugment, agent.ToolResolve, agent.FindingTrafficBindingEnabled
	prompt, wrapup, wrapupTurns := agent.PromptOverride, agent.WrapupOverride, agent.WrapupMaxTurnsOverride
	timeoutWrapup, timeoutWrapupTurns := agent.WrapupTaskTimeoutOverride, agent.WrapupTaskTimeoutTurnsOverride
	return func() {
		agent.ToolAugment, agent.ToolResolve, agent.FindingTrafficBindingEnabled = toolAugment, toolResolve, trafficBinding
		agent.PromptOverride, agent.WrapupOverride, agent.WrapupMaxTurnsOverride = prompt, wrapup, wrapupTurns
		agent.WrapupTaskTimeoutOverride, agent.WrapupTaskTimeoutTurnsOverride = timeoutWrapup, timeoutWrapupTurns
	}
}

// stopTaskRuntimes 는 엔진이 띄운 작업별 루프(planner·worker·마감 조정자)를 모두 멈추고
// 끝나기를 기다린다. 엔진 전체를 멈추는 공개 경로가 없어 작업마다 StopTask를 부른다.
func stopTaskRuntimes(e *Engine) {
	e.runtimeMu.Lock()
	taskIDs := make([]string, 0, len(e.runtimes))
	for taskID := range e.runtimes {
		taskIDs = append(taskIDs, taskID)
	}
	e.runtimeMu.Unlock()
	for _, taskID := range taskIDs {
		e.StopTask(taskID)
	}
}
