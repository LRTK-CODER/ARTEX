package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// e2eEnv 를 1로 두면 TestAgentSandboxEndToEnd가 돈다. 도우미에 파일 cap이 있고 테스트 실행 파일에
// cap_kill+p가 있는 컨테이너에서만 의미가 있다(README "에이전트 uid 분리", PR 본문의 컨테이너 확인 절차).
const (
	e2eEnv       = "ARTEX_SANDBOX_E2E"
	e2eHelperEnv = "ARTEX_SANDBOX_E2E_HELPER"
)

// e2eWait 는 프로세스가 시작하거나 끝나기를 기다리는 상한이다.
const e2eWait = 10 * time.Second

// mcpUIDServer 는 도구 이름에 자기 uid를 담아 돌려주는 최소 stdio MCP 서버다(줄 단위 JSON-RPC).
const mcpUIDServer = `import json, os, sys
for line in sys.stdin:
    msg = json.loads(line)
    if "id" not in msg:
        continue
    if msg["method"] == "initialize":
        result = {"protocolVersion": "2024-11-05", "capabilities": {}, "serverInfo": {"name": "e2e", "version": "1"}}
    elif msg["method"] == "tools/list":
        result = {"tools": [{"name": "uid_%d" % os.getuid(), "description": "uid", "inputSchema": {"type": "object", "properties": {}}}]}
    else:
        result = {}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}) + "\n")
    sys.stdout.flush()
`

// TestAgentSandboxEndToEnd 는 uid 분리를 켠 실제 컨테이너에서 에이전트 명령의 모든 실행 경로가 에이전트 uid로
// 돌고, 서버 비밀에 닿지 못하고, 시간 초과·TaskStop·shell_close가 에이전트 프로세스를 실제로 죽이는지 본다.
// 결과 출력은 t.Log로 남겨 PR의 컨테이너 확인 증거로 쓴다.
func TestAgentSandboxEndToEnd(t *testing.T) {
	if os.Getenv(e2eEnv) != "1" {
		t.Skipf("%s=1 and a container with the sandbox helper are required", e2eEnv)
	}
	ctx := context.Background()
	uid := os.Getenv(AgentUIDEnv)
	helper := os.Getenv(e2eHelperEnv)
	if helper == "" {
		helper = "/app/" + SandboxHelperName
	}
	// t.TempDir()은 0700 상위 디렉터리 아래에 만들어 에이전트가 들어가지 못한다. 데이터 디렉터리를 /tmp 바로 아래에 둔다.
	dataDir, err := os.MkdirTemp("", "artex-e2e-data-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) }) // 정리 실패는 컨테이너가 사라지면 함께 없어진다
	sb, err := EnableAgentSandbox(ctx, AgentSandboxConfig{UID: uid, GID: os.Getenv(AgentGIDEnv), Helper: helper, DataDir: dataDir})
	if err != nil || sb == nil {
		t.Fatalf("EnableAgentSandbox = %v, %v", sb, err)
	}
	if !strings.HasPrefix(os.Getenv("PATH"), sb.ShellDir()+":") {
		t.Fatalf("PATH does not start with the shell dir: %q", os.Getenv("PATH"))
	}
	ws := filepath.Join(dataDir, "sessions", "e2e")
	if err := os.MkdirAll(ws, 0o775); err != nil {
		t.Fatal(err)
	}
	tasks, err := actool.NewManager("e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tasks.Cleanup)
	syncCtx := &actool.ToolContext{WorkingDir: ws}
	managedCtx := &actool.ToolContext{WorkingDir: ws, Tasks: tasks}
	bash := actool.NewBash()

	t.Run("bash sync", func(t *testing.T) {
		out := runTool(t, bash, syncCtx, map[string]any{"command": "id -u; id; umask; grep -E 'CapEff|NoNewPrivs' /proc/self/status"})
		assertFirstLine(t, out, uid)
	})
	t.Run("bash managed", func(t *testing.T) {
		assertFirstLine(t, runTool(t, bash, managedCtx, map[string]any{"command": "id -u"}), uid)
	})
	t.Run("bash background and TaskStop", func(t *testing.T) {
		runTool(t, bash, managedCtx, map[string]any{"command": "id -u > bg_uid; exec sleep 301", "run_in_background": true})
		assertFileUID(t, filepath.Join(ws, "bg_uid"), uid)
		waitAgentProcess(t, uid, "sleep 301", true)
		for _, info := range tasks.List() {
			out := runTool(t, actool.NewTaskStop(), managedCtx, map[string]any{"task_id": info.ID})
			t.Logf("TaskStop: %s", out)
		}
		waitAgentProcess(t, uid, "sleep 301", false)
	})
	t.Run("bash timeout sync", func(t *testing.T) {
		t.Log(runTool(t, bash, syncCtx, map[string]any{"command": "sleep 302", "timeout_ms": 1000}))
		waitAgentProcess(t, uid, "sleep 302", false)
	})
	t.Run("bash timeout managed", func(t *testing.T) {
		t.Log(runTool(t, bash, managedCtx, map[string]any{"command": "sleep 304", "timeout_ms": 1000}))
		waitAgentProcess(t, uid, "sleep 304", false)
	})
	t.Run("shell_open and shell_close", func(t *testing.T) {
		out := runTool(t, actool.NewShellOpen(), managedCtx, map[string]any{"command": "id -u > shell_uid; exec sleep 303"})
		var opened struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(out), &opened); err != nil || opened.SessionID == "" {
			t.Fatalf("shell_open output %q: %v", out, err)
		}
		assertFileUID(t, filepath.Join(ws, "shell_uid"), uid)
		waitAgentProcess(t, uid, "sleep 303", true)
		t.Logf("shell_close: %s", runTool(t, actool.NewShellClose(), managedCtx, map[string]any{"session_id": opened.SessionID}))
		waitAgentProcess(t, uid, "sleep 303", false)
	})
	t.Run("custom command tool", func(t *testing.T) {
		res, err := (&Server{}).runCommandTool(ctx, json.RawMessage(`{"command":"id -u"}`), nil, syncCtx)
		if err != nil {
			t.Fatal(err)
		}
		assertFirstLine(t, res.Flatten(), uid)
	})
	t.Run("custom python tool", func(t *testing.T) {
		out, err := execPython(ctx, "python3", "e2e", "import os\nprint(os.getuid())\n", nil, ws, nil, e2eWait, sb)
		if err != nil {
			t.Fatal(err)
		}
		assertFirstLine(t, out, uid)
	})
	t.Run("stdio mcp", func(t *testing.T) {
		script := filepath.Join(ws, "mcp_uid.py")
		if err := os.WriteFile(script, []byte(mcpUIDServer), 0o644); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal([]string{script})
		cl, err := connectMCP(ctx, &db.MCPServer{Name: "e2e", Transport: "stdio", Command: "python3", Args: args}, sb)
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		tools, err := cl.Tools(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(tools) != 1 || !strings.HasSuffix(tools[0].Name(), "uid_"+uid) {
			t.Fatalf("mcp tools = %v, want one tool ending in uid_%s", toolNames(tools), uid)
		}
		t.Logf("mcp tool: %s", tools[0].Name())
	})
	t.Run("server secrets are out of reach", func(t *testing.T) {
		keyDir, err := PrepareKeyDir(filepath.Join(os.TempDir(), "artex-e2e-keys"), dataDir+"-base", dataDir)
		if err != nil {
			t.Fatal(err)
		}
		key := filepath.Join(keyDir, "oauth.key")
		if err := os.WriteFile(key, []byte("FAKE-KEY-165"), 0o600); err != nil {
			t.Fatal(err)
		}
		exeDir := filepath.Dir(mustExecutable(t))
		cmd := fmt.Sprintf("cat %s; cat /proc/%d/environ; touch %s; %s check; echo done", shellQuote(key), os.Getpid(), shellQuote(filepath.Join(exeDir, "artex.new")), shellQuote(helper))
		out := runTool(t, bash, syncCtx, map[string]any{"command": cmd})
		t.Log(out)
		if strings.Contains(out, "FAKE-KEY-165") {
			t.Fatalf("agent read the server key: %q", out)
		}
		// 읽기 전용 루트 파일 시스템이면 artex.new는 uid와 무관하게 "Read-only file system"으로 실패한다.
		for _, want := range []string{"oauth.key: Permission denied", "environ: Permission denied", "cannot touch", SandboxHelperName + ": Permission denied"} {
			if !strings.Contains(out, want) {
				t.Fatalf("agent command output lacks %q: %q", want, out)
			}
		}
		if _, err := os.Stat(filepath.Join(exeDir, "artex.new")); err == nil {
			t.Fatal("agent created artex.new next to the executable")
		}
	})
	t.Run("workspace is shared both ways", func(t *testing.T) {
		// 파일 도구가 쓴 뒤의 권한(0664, agent 패키지의 TestFileToolWritesShareWithGroup)과 같게 서버 uid로 쓴다.
		// 그룹은 setgid 작업 공간에서 물려받는다.
		if err := os.WriteFile(filepath.Join(ws, "by-server.txt"), []byte("server\n"), 0o664); err != nil {
			t.Fatal(err)
		}
		out := runTool(t, bash, syncCtx, map[string]any{"command": "echo agent >> by-server.txt && echo agent > by-agent.txt && ls -ln by-server.txt by-agent.txt && cat by-server.txt"})
		t.Log(out)
		data, err := os.ReadFile(filepath.Join(ws, "by-agent.txt"))
		if err != nil || string(data) != "agent\n" {
			t.Fatalf("server read of the agent's file: %q, %v", data, err)
		}
		if err := os.WriteFile(filepath.Join(ws, "by-agent.txt"), []byte("server edit\n"), 0o644); err != nil {
			t.Fatalf("server edit of the agent's file: %v", err)
		}
	})
	t.Run("nmap connect scan works and syn scan does not", func(t *testing.T) {
		if _, err := exec.LookPath("nmap"); err != nil {
			t.Skip("nmap is not installed")
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		connect := runTool(t, bash, syncCtx, map[string]any{"command": "nmap -Pn -p " + port + " 127.0.0.1"})
		t.Log(connect)
		if !strings.Contains(connect, port+"/tcp open") {
			t.Fatalf("nmap connect scan did not find the open port: %q", connect)
		}
		t.Log(runTool(t, bash, syncCtx, map[string]any{"command": "nmap -sS -Pn -p " + port + " 127.0.0.1"}))
	})
}

func runToolResult(t *testing.T, tool actool.CoreTool, tc *actool.ToolContext, input map[string]any) actool.Result {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(context.Background(), raw, tc)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return res
}

func runTool(t *testing.T, tool actool.CoreTool, tc *actool.ToolContext, input map[string]any) string {
	t.Helper()
	return runToolResult(t, tool, tc, input).Flatten()
}

func assertFirstLine(t *testing.T, out, want string) {
	t.Helper()
	t.Log(out)
	if first, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); strings.TrimSpace(first) != want {
		t.Fatalf("first line = %q, want uid %s (output %q)", first, want, out)
	}
}

func assertFileUID(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(e2eWait)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) == want {
			t.Logf("%s: uid %s", filepath.Base(path), want)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %q, %v, want uid %s", path, data, err, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitAgentProcess 는 uid로 돌고 명령줄에 marker가 든 프로세스가 생기거나(running=true) 사라지기를 기다린다.
func waitAgentProcess(t *testing.T, uid, marker string, running bool) {
	t.Helper()
	deadline := time.Now().Add(e2eWait)
	for {
		pids := agentProcesses(uid, marker)
		if (len(pids) > 0) == running {
			t.Logf("processes uid=%s %q: %v", uid, marker, pids)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes uid=%s %q = %v, want running=%v", uid, marker, pids, running)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func agentProcesses(uid, marker string) []string {
	var pids []string
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if procUID(string(status)) == uid && strings.Contains(strings.ReplaceAll(string(cmdline), "\x00", " "), marker) {
			pids = append(pids, e.Name())
		}
	}
	return pids
}

// procUID 는 /proc/<pid>/status의 "Uid:" 줄에서 실제 uid를 꺼낸다.
func procUID(status string) string {
	for _, line := range strings.Split(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if fields := strings.Fields(rest); len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func toolNames(tools []actool.CoreTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}
