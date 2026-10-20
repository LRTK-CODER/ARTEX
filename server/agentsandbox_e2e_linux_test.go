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
	"syscall"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// e2eEnv 를 1로 두면 TestAgentSandboxEndToEnd가 돈다. 도우미에 파일 cap이 있는 컨테이너에서, 서버처럼
// "artex-sandbox launch -- <테스트 실행 파일>"로 CAP_KILL을 ambient로 받아 실행할 때만 의미가 있다(README
// "에이전트 uid 분리", PR 본문의 컨테이너 확인 절차).
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

// TestAgentSandboxEndToEnd 는 uid 분리를 켠 실제 컨테이너에서 에이전트 명령의 모든 실행 경로가 그 실행의 uid로
// 돌고, 서버 비밀과 다른 실행의 프로세스에 닿지 못하고, uid를 싣지 못한 길은 실행되지 않고(#177), 시간 초과·
// TaskStop·shell_close가 에이전트 프로세스를 실제로 죽이는지 본다.
// 결과 출력은 t.Log로 남겨 PR의 컨테이너 확인 증거로 쓴다.
func TestAgentSandboxEndToEnd(t *testing.T) {
	if os.Getenv(e2eEnv) != "1" {
		t.Skipf("%s=1 and a container with the sandbox helper are required", e2eEnv)
	}
	ctx := context.Background()
	sharedUID, sharedGID := os.Getenv(AgentUIDEnv), os.Getenv(AgentGIDEnv)
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
	shellDir := dataDir + "-shell"
	t.Cleanup(func() { _ = os.RemoveAll(shellDir) })
	sb, err := EnableAgentSandbox(ctx, AgentSandboxConfig{UID: sharedUID, GID: sharedGID, Helper: helper, ShellDir: shellDir, DataDir: dataDir})
	if err != nil || sb == nil {
		t.Fatalf("EnableAgentSandbox = %v, %v", sb, err)
	}
	if got := os.Getenv("PATH"); got != sb.ShellDir() {
		t.Fatalf("PATH = %q, want only the shell dir %q", got, sb.ShellDir())
	}
	t.Logf("server caps after enable:\n%s", procCapLines(t, "/proc/self/status"))
	if got := os.Getenv(AgentUIDEnv) + os.Getenv(AgentGIDEnv); got != "" {
		t.Fatalf("shared agent uid/gid left in the server environment: %q", got)
	}
	// 작업·세션 실행은 할당한 uid와 같은 값의 gid로 돈다(#177, #182). DB 없이 풀의 첫 두 uid를 작업 A·B의 uid로
	// 쓴다(풀 사용자는 이미지에 있다). 작업 공간은 각자의 uid에만 ACL로 열린다.
	const uidA, uidB = db.AgentUIDPoolFirst, db.AgentUIDPoolFirst + 1
	uid, otherUID := strconv.Itoa(uidA), strconv.Itoa(uidB)
	envA, envB := identityEnv(uidA, uidA), identityEnv(uidB, uidB)
	ws, wsB := filepath.Join(dataDir, "sessions", "e2e"), filepath.Join(dataDir, "sessions", "e2e-b")
	for dir, owner := range map[string]int{ws: uidA, wsB: uidB} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := agent.ShareDir(dir, owner, agent.WorkspaceWrite); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := actool.NewManager("e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tasks.Cleanup)
	syncCtx := &actool.ToolContext{WorkingDir: ws, Env: envA}
	managedCtx := &actool.ToolContext{WorkingDir: ws, Tasks: tasks, Env: envA}
	bash := actool.NewBash()

	t.Run("bash sync", func(t *testing.T) {
		out := runTool(t, bash, syncCtx, map[string]any{"command": "id -u; id; umask; echo PATH=$PATH; grep -E 'CapEff|NoNewPrivs' /proc/self/status"})
		assertFirstLine(t, out, uid)
		// 도우미가 권한을 모두 버리고 no_new_privs를 켰는지, 원래 PATH를 되돌렸는지 본다.
		for _, want := range []string{"CapEff:\t0000000000000000", "NoNewPrivs:\t1", "PATH=" + os.Getenv(AgentPathEnv) + "\n"} {
			if !strings.Contains(out, want) {
				t.Fatalf("agent shell lacks %q: %q", want, out)
			}
		}
		if strings.Contains(out, sb.ShellDir()) {
			t.Fatalf("agent PATH still has the shell dir: %q", out)
		}
	})
	t.Run("server caps do not leak to unwrapped children", func(t *testing.T) {
		// 서버가 감싸지 않고 띄우는 자식(ripgrep 등)과 같은 방식으로 절대 경로 명령을 띄운다.
		out, err := exec.Command("/bin/grep", "^Cap", "/proc/self/status").CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		t.Logf("unwrapped child:\n%s", out)
		for _, line := range []string{"CapInh:\t0000000000000000", "CapPrm:\t0000000000000000", "CapEff:\t0000000000000000", "CapAmb:\t0000000000000000"} {
			if !strings.Contains(string(out), line) {
				t.Fatalf("unwrapped child lacks %q", line)
			}
		}
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
	t.Run("custom tool test run", func(t *testing.T) {
		// 시험 실행은 데이터 디렉터리가 아니라 sessions/ 아래 임시 작업 공간에서 돈다(pgTestCustomTool).
		srv := &Server{m: &Manager{dir: dataDir}, sandbox: sb}
		testWS, err := srv.toolTestWorkspace()
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(testWS)
		// 시험 실행은 작업·세션에 속하지 않아 공용 uid로 돈다(pgTestCustomTool의 ToolContext와 같다).
		testTC := &actool.ToolContext{WorkingDir: testWS, Env: sb.SharedEnv()}
		out, err := execPython(ctx, "python3", "test", "import os\nprint(os.getuid())\nopen('probe-py', 'w').write('x')\n", nil, testWS, testTC.Env, e2eWait, sb)
		if err != nil {
			t.Fatal(err)
		}
		assertFirstLine(t, out, sharedUID)
		res, err := srv.runCommandTool(ctx, json.RawMessage(`{"command":"id -u; ls . >/dev/null && touch probe-cmd && echo ok"}`), nil, testTC)
		if err != nil {
			t.Fatal(err)
		}
		assertFirstLine(t, res.Flatten(), sharedUID)
		for _, name := range []string{"probe-py", "probe-cmd"} {
			info, err := os.Stat(filepath.Join(testWS, name))
			if err != nil {
				t.Fatal(err)
			}
			if owner := strconv.FormatUint(uint64(info.Sys().(*syscall.Stat_t).Uid), 10); owner != sharedUID {
				t.Fatalf("%s owner = %s, want shared agent uid %s", name, owner, sharedUID)
			}
		}
	})
	t.Run("custom python tool", func(t *testing.T) {
		out, err := execPython(ctx, "python3", "e2e", "import os\nprint(os.getuid())\n", nil, ws, envA, e2eWait, sb)
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
		cl, err := connectMCP(ctx, &db.MCPServer{Name: "e2e", Transport: "stdio", Command: "python3", Args: args}, sb, envA)
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
		for _, want := range []string{"oauth.key: Permission denied", "environ: Permission denied", "cannot touch", filepath.Base(helper) + ": Permission denied"} {
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
		// 작업 공간의 기본 ACL을 물려받아 작업 uid가 고치고, 에이전트가 만든 파일은 기본 ACL의 서버 uid 항목으로 서버가 고친다.
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
	t.Run("startup re-applies ACLs to a legacy shared-group workspace", func(t *testing.T) {
		// #175 배치를 흉내 낸다. 작업 공간은 공용 그룹(공용 gid)의 setgid 디렉터리이고, 공용 uid가 그 안에 파일을 만들었다.
		gid, err := strconv.Atoi(sharedGID)
		if err != nil {
			t.Fatal(err)
		}
		taskDir := filepath.Join(dataDir, "tasks", "70")
		work := filepath.Join(taskDir, "work")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteE2E(t, filepath.Join(work, "notes.md"), "server notes\n")
		for path, mode := range map[string]os.FileMode{taskDir: os.ModeSetgid | 0o755, work: os.ModeSetgid | 0o775, filepath.Join(work, "notes.md"): 0o664} {
			if err := os.Chown(path, -1, gid); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
		shared := &actool.ToolContext{WorkingDir: work, Env: sb.SharedEnv()}
		t.Log(runTool(t, bash, shared, map[string]any{"command": "umask 002; echo legacy > legacy.txt && mkdir legacydir && echo inner > legacydir/inner.txt && ls -ln . legacydir"}))

		if err := prepareWorkspaceRoots(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := applyWorkspaceACL(dataDir, workspaceOwners{tasks: map[int64]int{70: uidA}}); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("getfacl", "-n", "-p", taskDir, work, filepath.Join(work, "notes.md"), filepath.Join(work, "legacy.txt")).CombinedOutput(); err == nil {
			t.Logf("getfacl after re-apply:\n%s", out)
		}
		ownerCtx := &actool.ToolContext{WorkingDir: work, Env: envA}
		out := runTool(t, bash, ownerCtx, map[string]any{"command": "id; cat notes.md legacy.txt legacydir/inner.txt; echo more >> notes.md && echo notes-ok; touch fresh && echo fresh-ok; echo x >> legacy.txt; touch legacydir/new; echo done"})
		t.Logf("task uid %d in the re-applied legacy workspace:\n%s", uidA, out)
		for _, want := range []string{"server notes", "legacy", "inner", "notes-ok", "fresh-ok", "legacy.txt: Permission denied", "legacydir/new': Permission denied"} {
			if !strings.Contains(out, want) {
				t.Fatalf("task uid output lacks %q: %q", want, out)
			}
		}
		probe := fmt.Sprintf("id -u; ls %[1]s; cat %[1]s/legacy.txt; touch %[1]s/x; echo done", shellQuote(work))
		for _, other := range []struct {
			name string
			tc   *actool.ToolContext
		}{
			{"task B", &actool.ToolContext{WorkingDir: wsB, Env: envB}},
			{"shared uid", &actool.ToolContext{WorkingDir: dataDir, Env: sb.SharedEnv()}},
		} {
			out := runTool(t, bash, other.tc, map[string]any{"command": probe})
			t.Logf("%s reaches for task A's re-applied workspace:\n%s", other.name, out)
			if strings.Count(out, "Permission denied") < 3 || strings.Contains(out, "\nlegacy\n") {
				t.Fatalf("%s reached task A's workspace: %q", other.name, out)
			}
		}
	})
	t.Run("a deleted conversation's workspace is closed to its uid", func(t *testing.T) {
		// 대화를 지우면 uid가 풀로 돌아가 다른 작업·세션이 다시 받는다. 지우기 전에 닫으므로 같은 uid로도 들어가지 못한다.
		srv := &Server{m: &Manager{dir: dataDir}, sandbox: sb, chatBusy: map[string]bool{}, stopTicker: newStopTicker}
		const conversationID = 77
		dir := filepath.Join(dataDir, "sessions", srv.convBusyKey(conversationID))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := agent.ShareDir(dir, uidA, agent.WorkspaceWrite); err != nil {
			t.Fatal(err)
		}
		convCtx := &actool.ToolContext{WorkingDir: dir, Env: envA}
		assertFirstLine(t, runTool(t, bash, convCtx, map[string]any{"command": "echo CONVERSATION-SECRET > s.txt && echo written"}), "written")
		release, err := srv.closeConversationWorkspace(conversationID, time.Now().Add(conversationStopWait))
		if err != nil {
			t.Fatal(err)
		}
		release()
		probe := fmt.Sprintf("id -u; ls %[1]s; cat %[1]s/s.txt; touch %[1]s/x; echo done", shellQuote(dir))
		out := runTool(t, bash, &actool.ToolContext{WorkingDir: dataDir, Env: envA}, map[string]any{"command": probe})
		t.Logf("uid %d after the conversation's workspace was closed:\n%s", uidA, out)
		if strings.Count(out, "Permission denied") < 3 || strings.Contains(out, "CONVERSATION-SECRET") {
			t.Fatalf("the returned uid still reaches the deleted conversation's workspace: %q", out)
		}
	})
	t.Run("a task deleted without its files is closed to its uid", func(t *testing.T) {
		// 파일을 남기고 작업을 지우면(화면의 기본값) uid가 풀로 돌아가 다른 작업·세션이 다시 받는다. 지우기 전에
		// tasks/<id>를 닫으므로 같은 uid로도 지운 작업의 작업 공간·첨부·큰 출력에 들어가지 못한다.
		srv := &Server{m: &Manager{dir: dataDir}, sandbox: sb}
		const deletedTask = 78
		taskDir := filepath.Join(dataDir, "tasks", strconv.Itoa(deletedTask))
		ws := filepath.Join(taskDir, "work")
		// 실행이 거는 것과 같게 tasks/<id>는 읽기, work는 읽기·쓰기로 연다(agent.ensureRunDir).
		for dir, access := range map[string]agent.WorkspaceAccess{taskDir: agent.WorkspaceRead, ws: agent.WorkspaceWrite} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := agent.ShareDir(dir, uidA, access); err != nil {
				t.Fatal(err)
			}
		}
		taskCtx := &actool.ToolContext{WorkingDir: ws, Env: envA}
		assertFirstLine(t, runTool(t, bash, taskCtx, map[string]any{"command": "echo TASK-SECRET > notes.md && echo written"}), "written")
		if err := srv.closeTaskWorkspace(strconv.Itoa(deletedTask)); err != nil {
			t.Fatal(err)
		}
		probe := fmt.Sprintf("id -u; ls %[1]s; ls %[2]s; cat %[2]s/notes.md; touch %[2]s/x; echo done", shellQuote(taskDir), shellQuote(ws))
		out := runTool(t, bash, &actool.ToolContext{WorkingDir: dataDir, Env: envA}, map[string]any{"command": probe})
		t.Logf("uid %d after the deleted task's workspace was closed:\n%s", uidA, out)
		if strings.Count(out, "Permission denied") < 4 || strings.Contains(out, "TASK-SECRET") {
			t.Fatalf("the returned uid still reaches the deleted task's workspace: %q", out)
		}
	})
	t.Run("runs of different tasks cannot reach each other's processes", func(t *testing.T) {
		managedB := &actool.ToolContext{WorkingDir: wsB, Tasks: tasks, Env: envB}
		assertFirstLine(t, runTool(t, bash, managedB, map[string]any{"command": "id -u"}), otherUID)
		runTool(t, bash, managedB, map[string]any{"command": "exec sleep 305", "run_in_background": true})
		waitAgentProcess(t, otherUID, "sleep 305", true)
		pidB := agentProcesses(otherUID, "sleep 305")[0]
		cmd := fmt.Sprintf("id -u; kill -0 %[1]s; cat /proc/%[1]s/environ; ls /proc/%[1]s/cwd/; ls /proc/%[1]s/fd/; echo done", pidB)
		out := runTool(t, bash, syncCtx, map[string]any{"command": cmd})
		assertFirstLine(t, out, uid)
		for _, want := range []string{"Operation not permitted", "environ: Permission denied", "cwd/': Permission denied", "fd/': Permission denied"} {
			if !strings.Contains(out, want) {
				t.Fatalf("task A reached task B's process (lacks %q): %q", want, out)
			}
		}
		for _, info := range tasks.List() {
			runTool(t, actool.NewTaskStop(), managedB, map[string]any{"task_id": info.ID})
		}
		waitAgentProcess(t, otherUID, "sleep 305", false)
	})
	t.Run("monitor does not run even with the run's uid", func(t *testing.T) {
		// norma Monitor는 ToolContext.Env를 넘기지 않아 실행의 uid가 도우미에 닿지 않는다. 서버 환경에는 공용 uid도
		// 없으므로 도우미가 거부하고 명령은 실행되지 않는다(fail closed). 켠 상태의 한계로 README에 적었다.
		// norma Monitor가 Env를 넘기게 되면 이 경우는 할당 uid로 도는 것을 기대하도록 바꾼다.
		res := runToolResult(t, actool.NewMonitor(), managedCtx, map[string]any{"command": "id -u > mon-uid"})
		t.Logf("monitor: isError=%v %q", res.IsError, res.Flatten())
		id, ok := strings.CutPrefix(res.Flatten(), "Started monitor ")
		if !ok {
			t.Fatalf("monitor did not start a task: %q", res.Flatten())
		}
		id, _, _ = strings.Cut(id, ".")
		info, done := tasks.Wait(ctx, id, e2eWait)
		if !done {
			t.Fatalf("monitor task %s did not finish", id)
		}
		out, err := tasks.Output(id, 4096)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("monitor task: status=%s output=%q", info.Status, out)
		if info.Status != actool.TaskFailed || !strings.Contains(out, AgentUIDEnv+" is not set") {
			t.Fatalf("monitor task = %s %q, want failed with the helper refusal", info.Status, out)
		}
		if _, err := os.Stat(filepath.Join(ws, "mon-uid")); !os.IsNotExist(err) {
			t.Fatalf("monitor command ran: %v", err)
		}
	})
	t.Run("a run without its uid does not run", func(t *testing.T) {
		// 배선이 빠진 길(실행의 uid가 없는 ToolContext·python 환경)은 공용 uid로 떨어지지 않고 도우미가 거부한다.
		bare := &actool.ToolContext{WorkingDir: ws}
		res := runToolResult(t, bash, bare, map[string]any{"command": "id -u > nouid-bash"})
		t.Logf("bash without uid: isError=%v %q", res.IsError, res.Flatten())
		out, err := execPython(ctx, "python3", "e2e", "import os\nopen('nouid-py', 'w').write(str(os.getuid()))\n", nil, ws, nil, e2eWait, sb)
		t.Logf("python without uid: %q, %v", out, err)
		for _, got := range []string{res.Flatten(), out} {
			if !strings.Contains(got, AgentUIDEnv+" is not set") {
				t.Fatalf("output lacks the helper refusal: %q", got)
			}
		}
		if matches, _ := filepath.Glob(filepath.Join(ws, "nouid-*")); len(matches) != 0 {
			t.Fatalf("commands ran without a uid: %v", matches)
		}
	})
	t.Run("nmap connect scan works and syn scan does not", func(t *testing.T) {
		// 켠 뒤 서버 PATH에는 셸 대체만 있으므로 에이전트 명령의 PATH에서 찾는다.
		if _, err := sb.LookPath("nmap"); err != nil {
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
		// 도우미가 지운 NMAP_PRIVILEGED를 에이전트가 다시 줘도 raw 소켓은 열리지 않아야 한다(cap 0, no_new_privs).
		syn := runTool(t, bash, syncCtx, map[string]any{"command": "NMAP_PRIVILEGED=1 nmap -sS -Pn -p " + port + " 127.0.0.1"})
		t.Log(syn)
		if !strings.Contains(syn, "raw socket") || strings.Contains(syn, port+"/tcp open") {
			t.Fatalf("SYN scan with NMAP_PRIVILEGED=1 did not fail on the raw socket: %q", syn)
		}
	})
	// 마지막에 둔다. 셸 대체 링크를 지운 뒤에는 어떤 경로도 명령을 실행하지 않아야 한다(서버 uid로 넘어가지 않는다).
	t.Run("missing shell substitute fails closed", func(t *testing.T) {
		if err := os.Remove(filepath.Join(sb.ShellDir(), "bash")); err != nil {
			t.Fatal(err)
		}
		calls := map[string]func() actool.Result{
			"bash sync": func() actool.Result {
				return runToolResult(t, bash, syncCtx, map[string]any{"command": "id -u > fc-sync"})
			},
			"bash background": func() actool.Result {
				return runToolResult(t, bash, managedCtx, map[string]any{"command": "id -u > fc-bg", "run_in_background": true})
			},
			"monitor": func() actool.Result {
				return runToolResult(t, actool.NewMonitor(), managedCtx, map[string]any{"command": "id -u > fc-monitor"})
			},
			"shell_open": func() actool.Result {
				return runToolResult(t, actool.NewShellOpen(), managedCtx, map[string]any{"command": "id -u > fc-shell"})
			},
			"command tool": func() actool.Result {
				res, err := (&Server{}).runCommandTool(ctx, json.RawMessage(`{"command":"id -u > fc-cmd"}`), nil, syncCtx)
				if err != nil {
					t.Fatal(err)
				}
				return res
			},
		}
		for name, call := range calls {
			res := call()
			t.Logf("%s: isError=%v %q", name, res.IsError, res.Flatten())
			if !res.IsError {
				t.Errorf("%s ran without the shell substitute", name)
			}
		}
		matches, _ := filepath.Glob(filepath.Join(ws, "fc-*"))
		if len(matches) != 0 {
			t.Fatalf("commands ran: %v", matches)
		}
	})
}

// procCapLines 는 /proc/<pid>/status의 Cap 줄과 NoNewPrivs 줄을 돌려준다.
func procCapLines(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Cap") || strings.HasPrefix(line, "NoNewPrivs") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
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

func mustWriteE2E(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
