package server

import (
	"context"
	"testing"
	"time"
)

// TestPlannerLoopReturnsPromptlyWhenCancelledDuringDebounce 는 planner 라운드가
// debounce 대기 중일 때 ctx가 취소되면 debounce가 끝나기를 기다리지 않고
// plannerLoop가 끝나는지 본다. StopTask의 rt.wg.Wait()가 이 반환을 기다린다(#81).
func TestPlannerLoopReturnsPromptlyWhenCancelledDuringDebounce(t *testing.T) {
	// debounce를 반환 상한보다 훨씬 길게 두어, 대기를 다 채운 반환과 바로 끝난 반환을
	// 시간 비교의 흔들림 없이 가른다.
	const debounce = 30 * time.Second
	const returnLimit = 2 * time.Second

	e := NewEngine(nil)
	e.debounce = debounce
	// 버퍼 없는 채널이라 두 번째 보내기가 받아들여졌다면 plannerLoop가
	// runRound의 debounce 대기 안에 있다는 뜻이다.
	task := &Task{ID: "t81", notify: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		e.plannerLoop(ctx, task)
		close(done)
	}()
	task.notify <- struct{}{} // 바깥 루프가 받아 runRound에 들어간다
	task.notify <- struct{}{} // debounce 대기가 받는다

	cancel()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(returnLimit):
		t.Fatalf("plannerLoop가 취소 뒤 %v 안에 끝나지 않았다(debounce %v를 기다리는 것으로 보인다)", returnLimit, debounce)
	}
	if elapsed := time.Since(start); elapsed >= returnLimit {
		t.Fatalf("취소 뒤 반환까지 %v 걸렸다, %v 미만이어야 한다", elapsed, returnLimit)
	}
}
