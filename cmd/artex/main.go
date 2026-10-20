// Command artex runs the ARTEX backend: the dual SQLite graph stores,
// the event-driven exploration engine, and the JSON HTTP API consumed by the
// shadcn/ui frontend.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/selfupdate"
	"github.com/Autumn-27/artex/server"
)

// version is the build version, injected at release time via
// -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

const banner = `
    _    ____ _____ _______  __
   / \  |  _ \_   _| ____\ \/ /
  / _ \ | |_) || | |  _|  \  /
 / ___ \|  _ < | | | |___ /  \
/_/   \_\_| \_\|_| |_____/_/\_\
`

// printBanner writes the startup banner + version/runtime info to stdout.
func printBanner(addr string) {
	fmt.Print(banner)
	fmt.Println("  AI 자율 침투 테스트 시스템")
	fmt.Printf("  버전 %s  ·  %s/%s  ·  %s  ·  수신 주소 %s\n\n",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version(), addr)
}

// main only maps run's result onto the process exit code. The exit code is part
// of the update protocol — the supervising start script reads it to decide
// whether to relaunch us (see selfupdate.ExitRestart) — so the body has to live
// in a function that can *return* rather than os.Exit past its own defers.
func main() {
	os.Exit(run())
}

func run() int {
	var (
		addr    = flag.String("addr", ":8787", "HTTP listen address")
		dataDir = flag.String("data", filepath.Join(config.BaseDir(), "data"), "data directory for SQLite stores (default: data/ next to the executable)")
		proxy   = flag.String("proxy", "127.0.0.1:8788", "traffic recording proxy address (empty to disable)")
	)
	flag.Parse()

	// 비밀 환경 변수(DB DSN, LLM 키)를 읽어 두고 이 프로세스 환경에서 지운다. 에이전트가 띄우는 자식 프로세스는
	// 서버 환경을 그대로 물려받으므로, 어떤 자식도 띄우기 전인 여기서 해야 한다. 업데이트 재시작은 이 프로세스가
	// 종료하고 감시 스크립트(start.sh)나 Docker가 자기 환경으로 다시 띄우므로 지운 값을 넘길 필요가 없다.
	secretEnv := config.TakeSecretEnv()

	// hand the build version to the server package so GET /api/health can report it
	// to the frontend top bar.
	server.BuildVersion = version

	printBanner(*addr)

	// capture backend logs into the in-memory sink (still to stderr) so the /logs
	// page can show a live log stream. Do this first, to catch startup logs too.
	server.StartLogCapture()

	// Self-update bootstrap: swap in a staged binary, or count a post-swap boot
	// attempt and roll back if the new build keeps dying. Must run before we open
	// the stores or bind a port — this may end with "exit and let the start script
	// relaunch me", and there is no point paying for either first.
	action, upState := selfupdate.Bootstrap()
	server.SetBootUpdateState(upState)
	if action == selfupdate.Restart {
		return selfupdate.ExitRestart
	}

	// surface which config file the binary reads (absolute, so `go run`'s relative
	// "config.json" — resolved against the CWD — is unambiguous).
	cfgPath := config.Path()
	if abs, e := filepath.Abs(cfgPath); e == nil {
		cfgPath = abs
	}
	if _, e := os.Stat(cfgPath); e == nil {
		log.Printf("[config] 설정 파일: %s", cfgPath)
	} else {
		log.Printf("[config] 설정 파일: %s (없음 — 환경 변수 ARTEX_PG_DSN만 시도함)", cfgPath)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, shutdown := shutdownContext(sigCtx)
	defer shutdown(agent.AbortShutdown)

	// 컨테이너는 바인드 마운트 소유권이 맞지 않으면 시작하지 못한다(이미지는 root가 아닌 사용자로 돈다).
	// 단독 실행 파일은 키 디렉터리 기본값이 실행 파일 옆이라 쓰기 검사를 하면 예전 설치가 막힐 수 있어 컨테이너에서만 본다.
	if selfupdate.InDocker() {
		if err := server.CheckDirAccess(*dataDir, os.Getenv(server.KeyDirEnv)); err != nil {
			log.Printf("[config] %v", err)
			if errors.Is(err, fs.ErrPermission) {
				log.Print(server.VolumeOwnershipHint(os.Getuid()))
			}
			return 1
		}
	}

	// 에이전트 명령을 서버와 다른 uid로 실행한다(기본 꺼짐). PATH(셸 대체 디렉터리 하나로)와 umask를 바꾸므로 에이전트가 자식 프로세스를
	// 띄우기 전, 저장소를 열고 작업을 이어 실행하기 전에 한다. 켰는데 준비하지 못하면 조용히 끄지 않고 멈춘다.
	// 켜면 EnableAgentSandbox가 프로세스 환경에서 공용 uid·gid를 지우므로 로그에 쓸 값을 먼저 읽는다.
	sharedUID, sharedGID := os.Getenv(server.AgentUIDEnv), os.Getenv(server.AgentGIDEnv)
	sandbox, err := server.EnableAgentSandbox(ctx, server.AgentSandboxConfig{
		UID:      sharedUID,
		GID:      sharedGID,
		Helper:   filepath.Join(config.BaseDir(), server.SandboxHelperName),
		ShellDir: filepath.Join(config.BaseDir(), server.SandboxShellDirName),
		DataDir:  *dataDir,
	})
	if err != nil {
		log.Printf("[sandbox] 에이전트 uid 분리를 켤 수 없다: %v", err)
		return 1
	}
	if sandbox != nil {
		log.Printf("[sandbox] 에이전트 명령을 작업·세션마다 할당한 uid·gid로 실행한다(공용 uid %s·gid %s는 설정 화면 명령에만, 셸 대체 %s)", sharedUID, sharedGID, sandbox.ShellDir())
	}

	mgr, err := server.NewManager(*dataDir, *proxy, secretEnv)
	if err != nil {
		log.Fatalf("open stores: %v", err)
	}
	defer mgr.Close()
	// 작업 공간을 작업·세션마다 그 uid에만 연다(#182). 작업을 이어 실행하기 전에 DB의 할당 표대로 ACL을 다시 건다.
	if err := server.PrepareAgentWorkspaces(ctx, mgr, sandbox); err != nil {
		log.Printf("[sandbox] 작업 공간 권한을 맞추지 못했다: %v", err)
		return 1
	}

	// Surviving this long means a freshly swapped-in build actually works, so drop
	// the upgrade marker and stop counting attempts. Until it fires, every boot
	// increments the count and a build that keeps dying gets rolled back.
	settle := time.AfterFunc(selfupdate.SettleDelay, selfupdate.Settle)
	defer settle.Stop()

	skillDir := config.SkillDir()
	if abs, err := filepath.Abs(skillDir); err == nil {
		skillDir = abs
	}
	log.Printf("[config] 스킬 디렉터리: %s", skillDir)
	keyDir, err := server.PrepareKeyDir(os.Getenv(server.KeyDirEnv), config.BaseDir(), *dataDir)
	if err != nil {
		log.Printf("[config] 키 디렉터리: %v", err)
		if errors.Is(err, server.ErrKeyDirInsideDataDir) {
			log.Printf("[config] 키가 파일 관리자로 노출되지 않게 환경 변수 %s를 작업 공간(-data) 밖 디렉터리로 정한다", server.KeyDirEnv)
		}
		return 1
	}
	log.Printf("[config] 키 디렉터리: %s", keyDir)
	srv := server.New(ctx, mgr, skillDir, *dataDir, keyDir, sandbox)
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("ARTEX %s backend listening on %s (data=%s, workers=%d)", version, *addr, *dataDir, mgr.Workers())
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Two ways out: a signal (normal stop → exit 0, the start script stops looping)
	// or a staged update / rollback (→ exit 75, the script relaunches us and the
	// bootstrap above installs the new build).
	code := 0
	select {
	case <-ctx.Done():
	case <-server.RestartRequested():
		code = selfupdate.ExitRestart
		shutdown(agent.AbortShutdown)
	}

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return code
}

// shutdownContext deliberately does not derive from signalCtx. If it did, the
// parent's plain context.Canceled could win the race before AbortShutdown was
// attached to the child, losing the diagnostic cause in every running Agent.
func shutdownContext(signalCtx context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, shutdown := context.WithCancelCause(context.Background())
	go func() {
		select {
		case <-signalCtx.Done():
			shutdown(agent.AbortShutdown)
		case <-ctx.Done():
		}
	}()
	return ctx, shutdown
}
