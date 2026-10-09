package server

import (
	"context"
	"testing"
)

// newTestServer 는 New 로 서버를 띄우고, 테스트가 끝날 때 서버가 띄운 백그라운드 작업을
// 멈추고 끝나기를 기다린다. New 는 DB 에 남은 미완료 작업의 planner·worker 루프를
// 복원하는데, 기다리지 않으면 그 루프가 t.TempDir 정리와 다음 테스트의 New(전역 훅
// 재설정)와 겹쳐 TempDir 정리 실패와 DATA RACE 가 난다.
// t.Cleanup 은 등록 역순으로 돌므로 이 정리는 먼저 만든 t.TempDir 의 삭제보다 앞선다.
func newTestServer(t *testing.T, m *Manager, skillDir, dataDir, keyDir string) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, m, skillDir, dataDir, keyDir)
	t.Cleanup(func() {
		cancel()
		stopTaskRuntimes(s.engine)
		s.archiveWG.Wait()
		<-s.side.done
	})
	return s
}

// stopTaskRuntimes 는 엔진이 띄운 작업별 루프(planner·worker·마감 조정자)를 모두 멈추고
// 끝나기를 기다린다. 엔진 전체를 멈추는 공개 경로가 없어 작업마다 StopTask 를 부른다.
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
