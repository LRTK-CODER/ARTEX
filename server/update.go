package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// 화면의 원클릭 업데이트를 맡는 HTTP 계층이다. 실제 다운로드·검증·교체 로직은 모두 selfupdate 패키지에 있고,
// 여기서는 인증 경계, 동시 실행 막기, 진행률 방송, 그리고 '이제 종료하라'를 main에 알리는 일만 한다.
//
// 재시작은 이 프로세스가 하지 않는다. 새 버전을 임시로 받아 둔 뒤 selfupdate.ExitRestart로 종료하면
// 감시 스크립트(start.sh / start.bat, Docker에서는 ENTRYPOINT)가 다시 띄운다.

// restartCh 는 업데이트 준비나 롤백이 끝나면 닫힌다. main은 이를 받고 ExitRestart로 종료한다.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested 는 '종료하고 감시 프로세스가 다시 띄우게 하라'는 요청이 오면 닫히는 channel을 돌려준다.
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState 는 이번 시작 때 selfupdate.Bootstrap이 내린 결론(업데이트 성공, 방금 롤백, 받아 둔 파일 폐기)이다.
// main이 넣어 주며, /api/update/check가 지난 업데이트의 결과를 프런트에 그대로 알리는 데 쓴다.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState 는 main이 시작할 때 한 번 호출한다.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache 는 GitHub 최신 버전 조회 결과를 캐시한다.
//
// 상단 막대의 '새 버전 있음' 안내는 페이지를 새로 불러올 때마다 조회하는데, 인증하지 않은 GitHub API는
// IP당 시간당 60회까지다. 캐시하지 않으면 탭을 몇 개 열거나 몇 번 새로 고치는 것만으로 한도를 다 써서,
// 정작 업데이트하려 할 때 조회하지 못한다. 사용자가 직접 '업데이트 확인'을 누르면 force로 캐시를 건너뛴다.
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch 는 조회 함수다. 테스트에서 바꿔 끼우려고만 둔다. nil이면 실제 GitHub를 조회한다.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// 실패 결과도 잠깐 캐시한다. 그러지 않으면 GitHub에 연결할 수 없을 때 페이지를 불러올 때마다 시간 초과를 기다린다.
	// 다만 TTL은 짧게 둬서 네트워크가 돌아오면 곧 스스로 회복한다.
	releaseErrTTL = 2 * time.Minute
	// 조회용 시간 제한이다. NewClient의 30분 제한은 전체 패키지 다운로드용이라 버전 조회에서 그만큼 기다릴 수 없다.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get 은 최신 Release를 돌려준다. 캐시에 적중하면 네트워크에 접근하지 않는다.
//
// 조회하는 동안 잠금을 계속 쥔다. 동시 요청은 각자 GitHub를 부르지 않고 같은 조회 결과를 줄 서서 기다린다
// (페이지를 막 불러올 때 여러 탭이 한꺼번에 조회하는 순간이 속도 제한에 가장 걸리기 쉽다).
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// 요청이 취소된 것(사용자가 탭을 닫음)은 GitHub 문제가 아니므로 캐시에 쓰지 않는다.
	// 쓰면 다음 방문자가 영문 모를 '취소됨' 오류를 받는다.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress 는 프런트에 보내는 진행률 하나다.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // 다운로드 단계에서만 뜻이 있고 나머지는 -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub 는 업데이트 한 번의 진행률을 들고 SSE 구독자에게 방송한다.
//
// running 은 상호 배제도 겸한다. 업데이트 중에 POST /api/update/apply가 다시 오면 바로 409를 돌려
// 두 goroutine이 같은 artex.new에 동시에 쓰지 않게 한다.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin 은 업데이트 권한을 잡는다. 이미 진행 중이면 false를 돌려준다.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "준비 중…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish 는 업데이트 한 번을 끝낸다. err가 nil이면 받아 두기에 성공해 재시작을 기다린다는 뜻이다.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "업데이트하지 못했습니다", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "새 버전이 준비됐습니다. 다시 시작하는 중…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout 은 h.mu를 쥔 채로 호출해야 한다. 구독자 channel은 버퍼가 있고 차면 버린다.
// 진행률은 버려도 되는 순간 정보이므로, 멈춘 SSE 연결 하나가 업데이트 자체를 막게 하면 안 된다.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck 는 GitHub의 최신 정식 버전을 조회해 현재 버전과 비교한다.
//
// 프런트도 api.github.com에 직접 연결하지만(GitHub의 CORS는 *) **이 API의 결과를 기준으로 삼는다**.
// 다운로드는 백엔드가 하므로 백엔드가 GitHub에 접근할 수 있어야 업데이트할 수 있다. 브라우저는 연결되는데
// 서버는 연결되지 않는 경우가 흔하다(서버가 내부망에 있거나 프록시를 브라우저에만 설정함). 그때는 업데이트가
// 반드시 실패하므로 확인 단계에서 바로 오류를 알리는 편이 낫다.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// 상단 막대 안내는 캐시를 쓴다(기본). 사용자가 '업데이트 확인'을 누르면 force=1로 원본을 다시 조회한다.
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 개발 빌드(dev, 접미사가 붙은 git describe)에는 비교할 버전 번호가 없다. 허용하면
		// 로컬에서 디버깅 중인 바이너리를 정식 버전으로 덮어쓸 뿐이므로 업데이트를 막는다.
		out["reason"] = fmt.Sprintf("현재 버전 %q은(는) 정식 릴리스가 아니어서 원클릭 업데이트를 쓸 수 없습니다", current)
	}
	writeJSON(w, 200, out)
}

// updateApply 는 새 버전을 내려받아 임시로 둔 뒤 프로세스를 종료해 감시 스크립트가 다시 시작하게 한다.
//
// 바로 202를 돌려주고 실제 일은 백그라운드 goroutine에서 한다. 전체 패키지 다운로드는 몇 분 걸릴 수 있어
// 요청에 묶어 두면 리버스 프록시 시간 초과로 끊긴다. 진행률은 /api/update/stream으로 보낸다.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// 캐시를 쓴다. 사용자가 화면에서 보고 확인한 바로 그 버전을 설치하기 위해서다.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("현재 버전 %q은(는) 정식 릴리스가 아니어서 원클릭 업데이트를 쓸 수 없습니다", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("이미 최신 버전입니다: %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "이미 진행 중인 업데이트가 있습니다")
		return
	}

	go func() {
		// 요청의 ctx가 아니라 일부러 s.ctx를 쓴다. HTTP 응답을 돌려주면 요청이 끝나므로
		// 그 ctx에 묶어 다운로드하면 바로 취소된다.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 업데이트 실패: %v", err)
			return
		}
		log.Printf("[update] %s → %s 받아 둠, 교체를 마치려고 곧 종료", current, rel.TagName)
		// 마지막 진행률을 프런트에 보낼 시간을 조금 둔 뒤 종료한다.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback 은 이전 버전(교체 전에 백업한 artex.old)으로 직접 되돌린다.
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "업데이트가 진행 중이어서 롤백할 수 없습니다")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 이전 버전으로 수동 롤백함, 전환을 마치려고 곧 종료")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream 은 업데이트 진행률을 SSE로 보낸다.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// 먼저 현재 상태를 한 건 보낸다. 페이지를 새로 고쳐도 진행 중인 업데이트를 바로 볼 수 있다.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
