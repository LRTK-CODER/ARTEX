// Package cliprov 는 구독 CLI(Claude Code, Codex)를 LLM provider 로 쓰기 위한
// 공통 하위 프로세스 기반이다. 프로토콜 처리는 provider 가 맡고, 이 패키지는
// 실행 격리(환경 변수 허용 목록, 빈 임시 작업 디렉터리, 프로세스 그룹 정리),
// 프로필별 동시 실행 상한, 오류 분류, 반환 도구 이름 검증만 한다.
package cliprov

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultMaxConcurrent 는 Config.MaxConcurrent 가 0 일 때의 프로필별 동시 실행 상한이다.
// Claude 프로세스 하나가 약 270~300MB 를 쓴다(#14 측정).
const DefaultMaxConcurrent = 3

// DefaultTimeout 은 Config.Timeout 이 0 일 때 호출 하나의 상한이다.
const DefaultTimeout = 10 * time.Minute

// stderrLimitBytes 는 오류 메시지 분류에 쓸 stderr 꼬리의 최대 크기다.
const stderrLimitBytes = 64 << 10

// inheritedEnvNames 는 부모 환경에서 그대로 물려주는 변수다. 자격 증명 위치(HOME),
// 실행 파일 탐색(PATH), 로캘, 프록시만 넘긴다. CODEX_HOME 처럼 프로필마다 다른 값은
// 물려받지 않고 Config.Env 로만 받는다.
var inheritedEnvNames = []string{
	"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TZ",
	"LANG", "LC_ALL", "LC_CTYPE",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	// Windows 에서 프로세스가 뜨는 데 필요한 변수다. 다른 OS 에서는 없어서 넘어가지 않는다.
	"SYSTEMROOT", "APPDATA", "LOCALAPPDATA", "USERPROFILE",
}

// forbiddenEnvNames 는 Config.Env 로도 넘기지 못하는 ARTEX 비밀값 변수다.
// ANTHROPIC_API_KEY 가 있으면 `claude -p` 는 구독 대신 API 키로 과금한다.
var forbiddenEnvNames = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// forbiddenEnvPrefix 는 ARTEX 자체 설정(DB DSN, LLM 키 등) 변수의 접두사다.
const forbiddenEnvPrefix = "ARTEX_"

// ErrTimeout 은 호출이 Config.Timeout 을 넘겨 프로세스 그룹을 끊었을 때 올린다.
// context.DeadlineExceeded 를 감싸지 않는다. llmpool 은 그 오류를 "사용자가 멈춤"으로
// 보고 전환하지 않는데, CLI 가 멈춘 것은 다음 프로필로 넘길 일이기 때문이다.
var ErrTimeout = errors.New("cliprov: CLI 호출 시간 초과")

// Config 는 프로필 하나의 CLI 실행 설정이다.
type Config struct {
	// Program 은 CLI 실행 파일 경로나 PATH 에서 찾을 이름이다.
	Program string
	// Env 는 허용 목록 밖에서 자식에게 줄 변수다(ARTEX 전용 CODEX_HOME,
	// CLAUDE_CODE_OAUTH_TOKEN 등). 부모 환경의 같은 이름 값을 덮는다.
	Env map[string]string
	// MaxConcurrent 는 이 프로필의 동시 실행 상한이다. 0 이면 DefaultMaxConcurrent.
	MaxConcurrent int
	// Timeout 은 호출 하나의 상한이다. 0 이면 DefaultTimeout.
	Timeout time.Duration
}

// Runner 는 프로필 하나의 CLI 를 실행한다. 프로필마다 하나를 만들어 공유한다.
// 동시에 써도 안전하다.
type Runner struct {
	program string
	env     []string
	timeout time.Duration
	slots   chan struct{}
}

// NewRunner 는 cfg 를 검증해 Runner 를 만든다. Program 이 비었거나
// Env 에 ARTEX 비밀값 변수가 있으면 오류를 올린다.
func NewRunner(cfg Config) (*Runner, error) {
	if cfg.Program == "" {
		return nil, errors.New("cliprov: Program 이 비었다")
	}
	for name := range cfg.Env {
		if isForbiddenEnv(name) {
			return nil, fmt.Errorf("cliprov: 자식에게 넘길 수 없는 환경 변수 %q", name)
		}
	}
	limit := cfg.MaxConcurrent
	if limit <= 0 {
		limit = DefaultMaxConcurrent
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Runner{
		program: cfg.Program,
		env:     childEnv(os.Environ(), cfg.Env),
		timeout: timeout,
		slots:   make(chan struct{}, limit),
	}, nil
}

func isForbiddenEnv(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, forbiddenEnvPrefix) {
		return true
	}
	for _, f := range forbiddenEnvNames {
		if upper == f {
			return true
		}
	}
	return false
}

// childEnv 는 parent 에서 허용 목록의 변수만 고르고 extra 를 덧붙인다.
func childEnv(parent []string, extra map[string]string) []string {
	allowed := make(map[string]bool, len(inheritedEnvNames))
	for _, n := range inheritedEnvNames {
		allowed[n] = true
	}
	var env []string
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !allowed[name] {
			continue
		}
		if _, overridden := extra[name]; overridden {
			continue
		}
		env = append(env, kv)
	}
	for name, value := range extra {
		env = append(env, name+"="+value)
	}
	return env
}

// Exit 는 끝난 프로세스의 결과다. 종료 코드가 0 이 아니어도 오류가 아니다.
// 정상 여부는 provider 가 프로토콜 결과로 판단한다(Claude 는 정상 tool_use 종료가
// 종료 코드 1 이다, #14).
type Exit struct {
	Code int
	// Stderr 는 stderr 의 마지막 stderrLimitBytes 바이트다.
	Stderr string
}

// Process 는 실행 중인 CLI 하나다. Stdout 을 EOF 까지 읽은 뒤 Wait 를 부른다.
// 중간에 그만 읽을 때도 Wait 를 불러야 슬롯과 임시 디렉터리가 풀린다.
type Process struct {
	// Stdin 은 자식의 표준 입력이다. 다 쓰면 닫는다.
	Stdin io.WriteCloser
	// Stdout 은 자식의 표준 출력이다. 자식과 그 자손이 모두 끝나면 EOF 가 된다.
	Stdout io.Reader
	// Dir 은 이 호출 전용 빈 작업 디렉터리다. Wait 가 지운다.
	Dir string

	stdout *os.File
	done   chan struct{}
	exit   Exit
	err    error
}

// Start 는 동시 실행 슬롯을 얻고 args 로 CLI 를 띄운다. 슬롯을 기다리는 동안
// ctx 가 끝나면 ctx 오류를 올린다. 인자는 셸을 거치지 않고 그대로 넘긴다.
func (r *Runner) Start(ctx context.Context, args []string) (*Process, error) {
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("cliprov: 동시 실행 슬롯 대기: %w", ctx.Err())
	}
	p, err := r.start(ctx, args)
	if err != nil {
		<-r.slots
		return nil, err
	}
	return p, nil
}

func (r *Runner) start(ctx context.Context, args []string) (*Process, error) {
	dir, err := os.MkdirTemp("", "artex-cli-*")
	if err != nil {
		return nil, fmt.Errorf("cliprov: 작업 디렉터리: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	cleanup := func() {
		cancel()
		_ = os.RemoveAll(dir) // 실패해도 임시 디렉터리라 OS 가 정리한다
	}

	cmd := exec.CommandContext(runCtx, r.program, args...)
	cmd.Dir = dir
	cmd.Env = r.env
	setProcessGroup(cmd)
	// 기본 Cancel 은 자식만 죽인다. MCP 서버 같은 손자까지 끝내려고 그룹을 죽인다.
	cmd.Cancel = func() error { return killProcessGroup(cmd) }

	// 파이프를 직접 만들어 *os.File 로 넘긴다. exec 가 복사 goroutine 을 두지 않으므로
	// cmd.Wait 가 손자가 쥔 파이프를 기다리지 않고 자식이 끝나는 즉시 돌아온다.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("cliprov: stdin 파이프: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW)
		cleanup()
		return nil, fmt.Errorf("cliprov: stdout 파이프: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		cleanup()
		return nil, fmt.Errorf("cliprov: stderr 파이프: %w", err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW

	if err := cmd.Start(); err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		cleanup()
		return nil, fmt.Errorf("cliprov: %s 실행: %w", r.program, err)
	}
	// 자식 쪽 끝은 자식이 가졌으니 부모 사본을 닫는다. 그래야 EOF 가 온다.
	closeAll(stdinR, stdoutW, stderrW)

	p := &Process{Stdin: stdinW, Stdout: stdoutR, Dir: dir, stdout: stdoutR, done: make(chan struct{})}
	stderr := &tailBuffer{limit: stderrLimitBytes}
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stderr, stderrR) // 읽기 오류는 그룹이 끝나 파이프가 닫힌 것뿐이다
		_ = stderrR.Close()
		close(stderrDone)
	}()

	// 자식이 끝나면 그룹에 남은 손자를 정리한다. 이 goroutine 은 자식이 끝나면 끝나고,
	// 자식은 runCtx 의 시간 초과로 반드시 끝난다.
	go func() {
		waitErr := cmd.Wait()
		_ = killProcessGroup(cmd) // 이미 모두 끝났으면 실패하는 것이 정상이다
		<-stderrDone
		p.exit = Exit{Code: cmd.ProcessState.ExitCode(), Stderr: stderr.String()}
		p.err = waitError(ctx, runCtx, waitErr)
		_ = stdinW.Close() // provider 가 이미 닫았으면 오류가 나는 것이 정상이다
		cleanup()
		<-r.slots
		close(p.done)
	}()
	return p, nil
}

// waitError 는 cmd.Wait 결과를 provider 에게 줄 오류로 바꾼다. 0 이 아닌 종료 코드는
// 오류가 아니다.
func waitError(parent, run context.Context, err error) error {
	if parent.Err() != nil {
		return fmt.Errorf("cliprov: 호출 취소: %w", parent.Err())
	}
	if errors.Is(run.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	var exitErr *exec.ExitError
	if err == nil || errors.As(err, &exitErr) {
		return nil
	}
	return fmt.Errorf("cliprov: 프로세스 대기: %w", err)
}

// Wait 는 프로세스와 그 그룹이 끝나기를 기다려 결과를 돌려준다. 읽지 않은 Stdout 은
// 닫아서 자식이 쓰기에 막히지 않게 한다. 오류는 취소, 시간 초과(ErrTimeout),
// 대기 실패일 때만 올린다.
func (p *Process) Wait() (Exit, error) {
	_ = p.stdout.Close() // 두 번째 Wait 의 닫기 실패는 무시해도 된다
	<-p.done
	return p.exit, p.err
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close() // 실패 경로 정리라 더 할 일이 없다
	}
}

// tailBuffer 는 마지막 limit 바이트만 남기는 쓰기 버퍼다.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   bytes.Buffer
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(data)
	if over := b.buf.Len() - b.limit; over > 0 {
		b.buf.Next(over)
	}
	return len(data), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
