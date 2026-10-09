package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa 는 norma v0.4.0 이 들여온 '모델이 이끄는 컨텍스트 압축' 방식으로, 플랫폼 실험 기능으로서 사용자가
// 시스템 설정에서 켜고 끈다. 내장 compaction 과 상호 배타적이다. noaadapter.Enable 이 유일한 진입점으로,
// 한 번에 컨텍스트 관리자(Compactor), Compress 도구, 상주 프롬프트 세 개를 건다. Enable 을 부르지 않으면 꺼짐이다
// (내장 compaction 이 평소대로 동작한다). 토글은 각 agent 가 주입한 noaEnabledFn 이 해석하며 run 마다 한 번 읽으므로,
// 전환은 이후 시작하는 run 에만 영향을 주고 agent 를 다시 만들 필요가 없다.

// enableNoa 는 해석기가 켜짐이라고 알리면 noa 를 opts 에 접속시킨다. archiveRoot 는 압축 원문을 영속화하는 기준 디렉터리로
// (전역 workDir 를 쓰며, 각 agent 가 <workDir>/noa 아래에 모여 작업/탐색 의도 디렉터리로 흩어지지 않는다),
// sessionID 가 그 아래 아카이브 하위 디렉터리 이름이 된다(전역 유일하므로 같은 기준 디렉터리 안에서 충돌하지 않는다).
//
// noa 는 실험 기능이다. 접속에 실패해도 실제 작업을 중단해서는 안 된다. 오류가 나면 onWarn 으로 알리고 내장 압축으로 되돌린다.
// 사용 설정에 성공하면 opts.Compaction 을 지워, agentcore 가 '컨텍스트 관리자 두 개가 동시에 설정됨' 으로 경고하는 것을 막는다.
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 압축 사용 설정 실패, 내장 압축으로 대체: " + err.Error())
		}
		return
	}
	// Compactor 가 Compaction 을 덮지만 둘이 함께 있으면 agentcore 가 매번 경고하므로 명시적으로 지운다.
	opts.Compaction = nil
}
