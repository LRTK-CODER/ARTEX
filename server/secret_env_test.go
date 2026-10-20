package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 서버 환경에 넣는 가짜 비밀값. 자식 환경 출력에 이 값이 하나라도 보이면 새는 것이다.
var fakeServerSecrets = map[string]string{
	"ARTEX_PG_DSN":       "postgres://artex:fake-pg-password@db:5432/artex",
	"ANTHROPIC_API_KEY":  "sk-ant-fake-anthropic-key",
	"OPENAI_API_KEY":     "sk-fake-openai-key",
	"ARTEX_LLM_PROXY":    "http://user:fake-llm-proxy-password@proxy:3128",
	"ARTEX_LLM_BASE_URL": "https://llm.example/fake-llm-base-token",
}

const (
	sessionProxyEnv = "HTTP_PROXY=http://session-proxy.test:8788" // 세션이 tc.Env로 넣는 프록시
	serverProxyEnv  = "HTTPS_PROXY=http://server-proxy.test:3128" // 서버 환경에 원래 있던 프록시
	serverPlainEnv  = "ARTEX_TEST_PLAIN=server-plain-value"       // 비밀이 아닌 서버 변수
)

// takeFakeServerSecrets 는 서버 시작을 흉내 낸다. 가짜 비밀값과 일반 변수를 환경에 넣고 TakeSecretEnv로 지운다.
// 실제 비밀 환경 변수(VM 테스트의 ARTEX_PG_DSN 등)도 TakeSecretEnv가 지우므로, 먼저 t.Setenv로 걸어
// 테스트가 끝나면 원래 값으로 돌아오게 한다.
func takeFakeServerSecrets(t *testing.T) config.SecretEnv {
	t.Helper()
	for name, value := range config.ReadSecretEnv() {
		t.Setenv(name, value)
	}
	for name, value := range fakeServerSecrets {
		t.Setenv(name, value)
	}
	for _, kv := range []string{serverProxyEnv, serverPlainEnv} {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}
	// norma 백그라운드 작업 디렉터리(os.TempDir()/norma)를 테스트 임시 디렉터리에 둔다.
	t.Setenv("TMPDIR", t.TempDir())
	return config.TakeSecretEnv()
}

func assertNoServerSecrets(t *testing.T, childEnv string) {
	t.Helper()
	for name, value := range fakeServerSecrets {
		if strings.Contains(childEnv, value) {
			t.Errorf("자식 환경에 %s 값이 있다:\n%s", name, childEnv)
		}
	}
}

func assertHasEnv(t *testing.T, childEnv string, kvs ...string) {
	t.Helper()
	for _, kv := range kvs {
		if !strings.Contains(childEnv, kv) {
			t.Errorf("자식 환경에 %q가 없다:\n%s", kv, childEnv)
		}
	}
}

// TestAgentChildProcessesDoNotInheritServerSecrets 는 시작 처리(TakeSecretEnv) 뒤 에이전트가 띄우는 자식 프로세스
// 경로마다 env 출력에 서버의 비밀값이 없고, 필요한 프록시·작업 변수는 남는지 확인한다.
func TestAgentChildProcessesDoNotInheritServerSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh·env·PTY가 필요하다")
	}
	takeFakeServerSecrets(t)
	sessionEnv := []string{sessionProxyEnv}

	tests := []struct {
		name     string
		run      func(t *testing.T) string
		wantEnvs []string
	}{
		{
			name: "Bash 동기 실행",
			run: func(t *testing.T) string {
				in, _ := json.Marshal(map[string]any{"command": "env"})
				res, err := actool.NewBash().Call(context.Background(), in, &actool.ToolContext{WorkingDir: t.TempDir(), Env: sessionEnv})
				if err != nil {
					t.Fatalf("Bash: %v", err)
				}
				return res.Flatten()
			},
			wantEnvs: []string{sessionProxyEnv, serverProxyEnv, serverPlainEnv},
		},
		{
			name: "Bash 백그라운드 작업",
			run: func(t *testing.T) string {
				tasks, err := actool.NewManager("secret-env-test")
				if err != nil {
					t.Fatalf("task manager: %v", err)
				}
				defer tasks.Cleanup()
				task, err := tasks.Spawn(actool.SpawnSpec{Command: "env", Kind: actool.KindBash, WorkingDir: t.TempDir(), Env: sessionEnv})
				if err != nil {
					t.Fatalf("spawn: %v", err)
				}
				if _, ok := tasks.Wait(context.Background(), task.ID, 30*time.Second); !ok {
					t.Fatalf("백그라운드 작업이 끝나지 않았다")
				}
				out, err := tasks.Output(task.ID, 1<<20)
				if err != nil {
					t.Fatalf("output: %v", err)
				}
				return out
			},
			wantEnvs: []string{sessionProxyEnv, serverProxyEnv, serverPlainEnv},
		},
		{
			name: "shell_open 대화형 세션",
			run: func(t *testing.T) string {
				tasks, err := actool.NewManager("secret-env-shell-test")
				if err != nil {
					t.Fatalf("task manager: %v", err)
				}
				defer tasks.Cleanup()
				in, _ := json.Marshal(map[string]any{"command": "env", "mode": "interactive", "quiet_ms": 300, "startup_grace_ms": 1})
				res, err := actool.NewShellOpen().Call(context.Background(), in, &actool.ToolContext{WorkingDir: t.TempDir(), Tasks: tasks, Env: sessionEnv})
				if err != nil {
					t.Fatalf("shell_open: %v", err)
				}
				return res.Flatten()
			},
			wantEnvs: []string{sessionProxyEnv, serverProxyEnv, serverPlainEnv},
		},
		{
			name: "stdio MCP",
			run: func(t *testing.T) string {
				out := filepath.Join(t.TempDir(), "env.txt")
				args, _ := json.Marshal([]string{"-c", "env > " + shellQuote(out)})
				env, _ := json.Marshal(map[string]string{"MCP_USER_TOKEN": "user-set-value"})
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				// sh는 MCP 서버가 아니라서 핸드셰이크는 실패한다. 프로세스가 끝나 stdout이 닫힌 뒤에 오류가 오므로
				// 그때는 env 파일이 다 써져 있다.
				if c, err := connectMCP(ctx, &db.MCPServer{Name: "secret-env", Transport: "stdio", Command: "sh", Args: args, Env: env}, nil); err == nil {
					c.Close()
				}
				b, err := os.ReadFile(out)
				if err != nil {
					t.Fatalf("MCP 자식 env 파일: %v", err)
				}
				return string(b)
			},
			// 사용자가 MCP 설정 env에 명시한 값은 계속 넘어간다.
			wantEnvs: []string{"MCP_USER_TOKEN=user-set-value", serverProxyEnv, serverPlainEnv},
		},
		{
			name: "사용자 지정 command 도구",
			run: func(t *testing.T) string {
				execRaw, _ := json.Marshal(map[string]any{"command": "env"})
				res, err := (&Server{}).runCommandTool(context.Background(), execRaw, nil, &actool.ToolContext{WorkingDir: t.TempDir(), Env: sessionEnv})
				if err != nil {
					t.Fatalf("command tool: %v", err)
				}
				return res.Flatten()
			},
			wantEnvs: []string{sessionProxyEnv, serverProxyEnv, serverPlainEnv},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			childEnv := tc.run(t)
			assertNoServerSecrets(t, childEnv)
			assertHasEnv(t, childEnv, tc.wantEnvs...)
		})
	}
}

// TestExecPythonPassesOnlyAllowlistedEnv 는 사용자 지정 python 도구가 서버 환경에서 허용 목록의 변수만 받고,
// 세션 프록시 env와 TOOL_* 파라미터는 계속 받는지 확인한다. 비밀값을 지우기 전의 환경으로 돌려,
// 지우기와 상관없이 허용 목록만으로도 비밀값과 서버 설정 변수가 빠지는지 본다.
func TestExecPythonPassesOnlyAllowlistedEnv(t *testing.T) {
	interp, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for name, value := range fakeServerSecrets {
		t.Setenv(name, value)
	}
	for _, kv := range []string{serverProxyEnv, serverPlainEnv, "LC_ALL=C.UTF-8", "no_proxy=localhost"} {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}
	code := "import os\nfor k, v in sorted(os.environ.items()):\n    print(k + '=' + v)\n"
	out, err := execPython(context.Background(), interp, "envdump", code,
		map[string]any{"target": "example.com"}, t.TempDir(), []string{sessionProxyEnv}, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("execPython: %v", err)
	}
	assertNoServerSecrets(t, out)
	if strings.Contains(out, serverPlainEnv) {
		t.Errorf("허용 목록 밖의 서버 변수가 넘어갔다:\n%s", out)
	}
	assertHasEnv(t, out, sessionProxyEnv, serverProxyEnv, "LC_ALL=C.UTF-8", "no_proxy=localhost", "TOOL_TARGET=example.com", "PATH=")
}
