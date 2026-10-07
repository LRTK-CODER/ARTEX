//go:build unix

// unix 태그: 손자를 Setsid 로 새 세션에 띄워 프로세스 그룹을 벗어나게 하는 일은 유닉스 계열에만 있다.

package cliprov

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv 는 테스트 바이너리를 가짜 손자로 다시 띄울 때 역할을 알리는 변수다.
const helperEnv = "CLIPROV_TEST_HELPER"

const (
	// helperLaunch 는 stderr 를 쥐는 손자를 그룹 밖에 띄우고 곧바로 끝나는 역할이다.
	helperLaunch = "launch"
	// helperLaunchStdout 은 stdout 을 쥐는 손자를 그룹 밖에 띄우고 곧바로 끝나는 역할이다.
	helperLaunchStdout = "launch-stdout"
	// helperHold 는 stderr 를 쥔 채 읽는 쪽이 닫힐 때까지(최대 holdLimit) 사는 역할이다.
	helperHold = "hold"
	// helperHoldStdout 은 stdout 을 쥔 채 읽는 쪽이 닫힐 때까지(최대 holdLimit) 사는 역할이다.
	helperHoldStdout = "hold-stdout"
)

// holdLimit 은 그룹을 벗어난 손자가 사는 최대 시간이다. 수정 전 Wait 는 이만큼 막힌다.
const holdLimit = 10 * time.Second

// heldLine 은 손자가 쥔 출력에 주기적으로 쓰는 줄이다. 읽는 쪽이 닫히면 쓰기가 실패해 손자가 끝난다.
const heldLine = "held\n"

// waitBound 는 시간 상한이나 정상 종료 뒤 Wait 가 돌아와야 하는 상한이다.
// 유예(수 초)를 넘겨 holdLimit 까지 막히면 실패한다.
const waitBound = 5 * time.Second

// TestEscapedGrandchildHelper 는 테스트가 아니라 가짜 CLI 가 띄우는 손자 프로세스다.
// helperEnv 가 없으면 아무것도 하지 않는다.
func TestEscapedGrandchildHelper(t *testing.T) {
	switch role := os.Getenv(helperEnv); role {
	case helperLaunch, helperLaunchStdout:
		cmd := exec.Command(os.Args[0], "-test.run=^TestEscapedGrandchildHelper$")
		// 손자는 한 스트림만 쥔다. 쥐지 않은 쪽은 /dev/null 이라 그 스트림의 EOF 를 막지 않는다.
		// 두 스트림을 함께 쥐면 stderr 를 닫을 때 손자가 끝나 stdout 도 풀려, stdout 을 닫는지 가릴 수 없다.
		if role == helperLaunch {
			cmd.Env = append(os.Environ(), helperEnv+"="+helperHold)
			cmd.Stderr = os.Stderr
		} else {
			cmd.Env = append(os.Environ(), helperEnv+"="+helperHoldStdout)
			cmd.Stdout = os.Stdout
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case helperHold:
		hold(os.Stderr)
	case helperHoldStdout:
		hold(os.Stdout)
	}
}

// hold 는 out 을 쥔 채 holdLimit 동안 산다. 읽는 쪽이 닫히면 쓰기가 실패하거나 SIGPIPE 로 끝난다.
func hold(out *os.File) {
	deadline := time.Now().Add(holdLimit)
	for time.Now().Before(deadline) {
		if _, err := out.WriteString(heldLine); err != nil {
			os.Exit(0)
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Exit(0)
}

// escapingCLI 는 그룹을 벗어나 stderr 를 쥐는 손자를 띄운 뒤 tail 을 실행하는 가짜 CLI 다.
func escapingCLI(t *testing.T, tail string) Config {
	t.Helper()
	return escapingCLIWith(t, helperLaunch, tail)
}

// escapingCLIWith 는 launchRole 로 그룹을 벗어난 손자를 띄운 뒤 tail 을 실행하는 가짜 CLI 다.
func escapingCLIWith(t *testing.T, launchRole, tail string) Config {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Program: fakeCLI(t, "'"+bin+"' -test.run='^TestEscapedGrandchildHelper$'\n"+tail),
		Env: map[string]string{
			helperEnv: launchRole,
			// -race 로 빌드한 바이너리는 끝날 때 1초 쉰다. 손자를 띄우는 쪽이 상한 전에 끝나게 끈다.
			"GORACE": "atexit_sleep_ms=0",
		},
	}
}

// waitWithin 은 Wait 가 waitBound 안에 돌아오는지 보고 걸린 시간과 결과를 돌려준다.
func waitWithin(t *testing.T, p *Process) (Exit, error, time.Duration) {
	t.Helper()
	type result struct {
		exit Exit
		err  error
	}
	start := time.Now()
	ch := make(chan result, 1)
	go func() {
		exit, err := p.Wait()
		ch <- result{exit, err}
	}()
	select {
	case r := <-ch:
		return r.exit, r.err, time.Since(start)
	case <-time.After(waitBound):
		t.Fatalf("Wait 가 %s 안에 돌아오지 않았다: 그룹을 벗어난 손자가 쥔 stderr 를 기다린다", waitBound)
		return Exit{}, nil, 0
	}
}

func TestRunnerTimeoutReturnsDespiteEscapedGrandchild(t *testing.T) {
	if testing.Short() {
		t.Skip("시간 상한과 유예를 실제로 기다려 수 초 걸린다")
	}
	cfg := escapingCLI(t, "echo escaped\nsleep 300")
	// 손자를 띄우는 테스트 바이너리가 뜨기 전에 상한이 오지 않게 1초를 둔다.
	cfg.Timeout = time.Second
	r := newRunner(t, cfg)
	p, err := r.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 이 줄이 오면 손자는 이미 새 세션에 있다. 오지 않으면 상한이 손자를 띄우기 전에 왔다.
	line := make([]byte, len("escaped\n"))
	if _, err := io.ReadFull(p.Stdout, line); err != nil {
		t.Fatalf("손자를 띄우기 전에 끝났다: %v", err)
	}
	_, err, took := waitWithin(t, p)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, 기대 ErrTimeout", err)
	}
	if _, statErr := os.Stat(p.Dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("Wait 뒤에도 작업 디렉터리가 남았다: %v", statErr)
	}
	t.Logf("Wait 걸린 시간 %s", took)
}

func TestRunnerCancelAfterNormalExitKeepsResult(t *testing.T) {
	if testing.Short() {
		t.Skip("유예를 실제로 기다려 수 초 걸린다")
	}
	// 그룹을 벗어난 손자가 stderr 를 쥐어, 자식이 끝난 뒤 결과를 정하기 전에 취소할 틈을 만든다.
	r := newRunner(t, escapingCLI(t, "echo done"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := r.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// stdout 의 EOF 는 자식이 끝나며 stdout 을 닫았다는 뜻이다(손자의 stdout 은 /dev/null).
	if out := readAll(t, p); out != "done\n" {
		t.Fatalf("stdout = %q", out)
	}
	cancel()
	exit, err, _ := waitWithin(t, p)
	if err != nil {
		t.Fatalf("정상 종료한 결과가 오류로 바뀌었다: %v", err)
	}
	if exit.Code != 0 {
		t.Errorf("종료 코드 = %d, 기대 0", exit.Code)
	}
}

// readWithin 은 Stdout 을 읽기가 끝날 때까지 읽고, 읽은 것과 끝낸 오류, 걸린 시간을 돌려준다.
// bound 안에 끝나지 않으면 실패한다. 손자가 쥔 stdout 은 holdLimit 까지 EOF 가 오지 않는다.
func readWithin(t *testing.T, p *Process, bound time.Duration) (string, error, time.Duration) {
	t.Helper()
	type result struct {
		out []byte
		err error
	}
	start := time.Now()
	ch := make(chan result, 1)
	go func() {
		out, err := io.ReadAll(p.Stdout)
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		return string(r.out), r.err, time.Since(start)
	case <-time.After(bound):
		t.Fatalf("Stdout 읽기가 %s 안에 끝나지 않았다: 그룹을 벗어난 손자가 쥔 stdout 을 기다린다", bound)
		return "", nil, 0
	}
}

func TestRunnerStdoutReadEndsDespiteEscapedGrandchild(t *testing.T) {
	if testing.Short() {
		t.Skip("시간 상한과 유예를 실제로 기다려 수 초 걸린다")
	}
	tests := []struct {
		name string
		// tail 은 손자를 띄운 뒤 가짜 CLI 가 실행할 셸 명령이다.
		tail    string
		timeout time.Duration
		// wantErr 는 Wait 가 올려야 할 오류다. nil 이면 정상 종료 결과를 기대한다.
		wantErr error
	}{
		// 이슈 #27: 자식이 상한에 걸려 죽어도 손자가 stdout 을 쥐어 읽기가 끝나지 않았다.
		{name: "시간 상한", tail: "echo escaped\nsleep 300", timeout: time.Second, wantErr: ErrTimeout},
		// 자식이 스스로 끝나도 손자가 쥔 stdout 때문에 읽기가 상한을 넘겨 막히지 않는다.
		{name: "정상 종료", tail: "echo escaped", timeout: 2 * time.Second, wantErr: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := escapingCLIWith(t, helperLaunchStdout, tc.tail)
			cfg.Timeout = tc.timeout
			r := newRunner(t, cfg)
			p, err := r.Start(context.Background(), nil)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			// 상한과 유예를 넘기되 손자가 사는 holdLimit 보다는 일찍 끝나야 한다.
			out, readErr, took := readWithin(t, p, tc.timeout+waitBound)
			t.Logf("Stdout 읽기 %s, err=%v", took, readErr)
			// 손자가 holdLimit 까지 stdout 을 쥐므로 EOF 가 아니라 유예 뒤 닫혀 끝나야 한다.
			if !errors.Is(readErr, os.ErrClosed) {
				t.Errorf("Stdout 읽기 오류 = %v, 기대 os.ErrClosed", readErr)
			}
			// 손자의 줄과 섞이므로 순서는 보지 않는다. 이 줄이 없으면 손자를 띄우기 전에 상한이 왔다.
			if !strings.Contains(out, "escaped\n") {
				t.Fatalf("stdout 에 자식 출력이 없다: %q", out)
			}
			exit, err, _ := waitWithin(t, p)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("정상 종료한 결과가 오류로 바뀌었다: %v", err)
				}
				if exit.Code != 0 {
					t.Errorf("종료 코드 = %d, 기대 0", exit.Code)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, 기대 %v", err, tc.wantErr)
			}
		})
	}
}
