package cliprov

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// hangGuard 는 프로세스 정리가 실패해 읽기가 끝나지 않을 때 테스트가 멈추지 않게 하는 상한이다.
const hangGuard = 10 * time.Second

// fakeCLI 는 body 를 본문으로 하는 가짜 CLI 셸 스크립트를 t.TempDir() 에 쓰고 경로를 돌려준다.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("가짜 CLI 가 /bin/sh 스크립트라 Windows 에서는 돌지 않는다")
	}
	path := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func newRunner(t *testing.T, cfg Config) *Runner {
	t.Helper()
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// readAll 은 Stdout 을 EOF 까지 읽는다. hangGuard 안에 끝나지 않으면 실패한다.
// EOF 는 자식과 stdout 을 물려받은 손자가 모두 끝나야 온다.
func readAll(t *testing.T, p *Process) string {
	t.Helper()
	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := io.ReadAll(p.Stdout)
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("stdout 읽기: %v", r.err)
		}
		return string(r.out)
	case <-time.After(hangGuard):
		t.Fatal("stdout 이 EOF 에 이르지 않았다: 프로세스 그룹이 남아 있다")
		return ""
	}
}

func run(t *testing.T, r *Runner, stdin string) (string, Exit, error) {
	t.Helper()
	p, err := r.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := io.WriteString(p.Stdin, stdin); err != nil {
		t.Fatalf("stdin 쓰기: %v", err)
	}
	if err := p.Stdin.Close(); err != nil {
		t.Fatalf("stdin 닫기: %v", err)
	}
	out := readAll(t, p)
	exit, err := p.Wait()
	return out, exit, err
}

func TestRunnerChildEnvIsAllowList(t *testing.T) {
	secrets := map[string]string{
		"ANTHROPIC_API_KEY":       "sk-ant-fake",
		"OPENAI_API_KEY":          "sk-openai-fake",
		"ARTEX_PG_DSN":            "postgres://u:fake@db/artex",
		"ARTEX_LLM_API_KEY":       "fake-llm-key",
		"CLAUDE_CODE_OAUTH_TOKEN": "fake-parent-token",
		"DATABASE_URL":            "postgres://fake",
	}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	t.Setenv("CODEX_HOME", "/home/user/.codex")
	t.Setenv("HOME", "/home/user")
	artexCodexHome := filepath.Join(t.TempDir(), "codex-home")

	r := newRunner(t, Config{
		Program: fakeCLI(t, "env"),
		Env:     map[string]string{"CODEX_HOME": artexCodexHome},
	})
	out, _, err := run(t, r, "")
	if err != nil {
		t.Fatalf("실행: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			got[name] = value
		}
	}

	for name := range secrets {
		t.Run("없음 "+name, func(t *testing.T) {
			if v, ok := got[name]; ok {
				t.Errorf("자식 환경에 %s=%q 가 있다", name, v)
			}
		})
	}
	wants := map[string]string{
		"CODEX_HOME": artexCodexHome, // 사용자 기본 CODEX_HOME 을 물려받지 않는다
		"HOME":       "/home/user",
		"PATH":       os.Getenv("PATH"),
	}
	for name, want := range wants {
		t.Run("있음 "+name, func(t *testing.T) {
			if got[name] != want {
				t.Errorf("%s = %q, 기대 %q", name, got[name], want)
			}
		})
	}
}

func TestNewRunnerRejectsSecretEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{"Anthropic 키", map[string]string{"ANTHROPIC_API_KEY": "x"}, true},
		{"OpenAI 키", map[string]string{"OPENAI_API_KEY": "x"}, true},
		{"ARTEX 접두사", map[string]string{"ARTEX_PG_DSN": "x"}, true},
		{"소문자도 거부", map[string]string{"artex_llm_api_key": "x"}, true},
		{"프로필 토큰은 허용", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x", "CODEX_HOME": "/x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRunner(Config{Program: "cli", Env: tc.env})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, 오류 기대 %v", err, tc.wantErr)
			}
		})
	}
}

func TestRunnerWorkDirIsEmptyTemp(t *testing.T) {
	r := newRunner(t, Config{Program: fakeCLI(t, `pwd -P; ls -A; echo end`)})
	p, err := r.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	dir := p.Dir
	// macOS 의 임시 디렉터리는 심볼릭 링크(/var → /private/var)라 pwd -P 와 맞춰 본다.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	out := readAll(t, p)
	if _, err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if want := realDir + "\nend\n"; out != want {
		t.Errorf("작업 디렉터리 출력 = %q, 기대 %q(빈 디렉터리)", out, want)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Wait 뒤에도 작업 디렉터리가 남았다: %v", err)
	}
}

// 종료 코드는 provider 가 판단한다. Claude 의 정상 tool_use 종료는 종료 코드 1 이다(#14).
func TestRunnerNonZeroExitIsNotError(t *testing.T) {
	resultLine := `{"type":"result","subtype":"error_max_turns","is_error":true,"stop_reason":"tool_use"}`
	r := newRunner(t, Config{Program: fakeCLI(t, "cat\necho '"+resultLine+"'\necho 'Reached maximum number of turns (1)' >&2\nexit 1")})

	out, exit, err := run(t, r, "prompt\n")
	if err != nil {
		t.Fatalf("0 이 아닌 종료 코드가 오류가 됐다: %v", err)
	}
	if exit.Code != 1 {
		t.Errorf("종료 코드 = %d, 기대 1", exit.Code)
	}
	if want := "prompt\n" + resultLine + "\n"; out != want {
		t.Errorf("stdout = %q, 기대 %q", out, want)
	}
	if !strings.Contains(exit.Stderr, "Reached maximum number of turns") {
		t.Errorf("stderr = %q", exit.Stderr)
	}
}

func TestRunnerTimeoutKillsProcessGroup(t *testing.T) {
	// 손자(MCP 서버 역할)가 stdout 을 물려받아 쥐고 있으므로, 손자까지 끝나야 EOF 가 온다.
	r := newRunner(t, Config{
		Program: fakeCLI(t, "sh -c 'sleep 300' &\nsleep 300"),
		Timeout: 200 * time.Millisecond,
	})
	_, _, err := run(t, r, "")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, 기대 ErrTimeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("시간 초과 오류가 DeadlineExceeded 를 감싸면 llmpool 이 전환하지 않는다")
	}
}

func TestRunnerExitKillsLeftoverGrandchild(t *testing.T) {
	// 자식은 정상 종료하지만 손자가 남는 경우. 자식이 끝나면 그룹을 정리해야 한다.
	r := newRunner(t, Config{Program: fakeCLI(t, "sh -c 'sleep 300' &\necho done")})
	out, exit, err := run(t, r, "")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if out != "done\n" || exit.Code != 0 {
		t.Errorf("stdout = %q, 종료 코드 = %d", out, exit.Code)
	}
}

func TestRunnerCancelReturnsContextError(t *testing.T) {
	r := newRunner(t, Config{Program: fakeCLI(t, "echo started\nsleep 300")})
	ctx, cancel := context.WithCancel(context.Background())
	p, err := r.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	buf := make([]byte, len("started\n"))
	if _, err := io.ReadFull(p.Stdout, buf); err != nil {
		t.Fatalf("첫 줄 읽기: %v", err)
	}
	cancel()
	if _, err := p.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, 기대 context.Canceled", err)
	}
}

func TestRunnerLimitsConcurrency(t *testing.T) {
	r := newRunner(t, Config{Program: fakeCLI(t, "cat"), MaxConcurrent: 1})
	first, err := r.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("첫 Start: %v", err)
	}

	// 슬롯이 찼으므로 끝난 ctx 로는 기다리지 못하고 ctx 오류가 난다.
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Start(done, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("슬롯이 찼는데 Start err = %v, 기대 context.Canceled", err)
	}

	if err := first.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
	readAll(t, first)
	if _, err := first.Wait(); err != nil {
		t.Fatalf("첫 Wait: %v", err)
	}

	second, err := r.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("슬롯이 풀린 뒤 Start: %v", err)
	}
	if err := second.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
	readAll(t, second)
	if _, err := second.Wait(); err != nil {
		t.Fatalf("둘째 Wait: %v", err)
	}
}

func TestRunnerStartFailureReleasesSlot(t *testing.T) {
	r := newRunner(t, Config{Program: filepath.Join(t.TempDir(), "missing-cli"), MaxConcurrent: 1})
	for i := range 2 {
		if _, err := r.Start(context.Background(), nil); err == nil {
			t.Fatalf("%d번째 Start: 없는 실행 파일인데 오류가 없다", i+1)
		} else if errors.Is(err, context.Canceled) {
			t.Fatalf("%d번째 Start 가 슬롯을 기다렸다: %v", i+1, err)
		}
	}
}
