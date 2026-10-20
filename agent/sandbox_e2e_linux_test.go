package agent

import (
	"fmt"
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

// sandboxE2EEnv 를 1로 두면 TestSharedWorkspaceEndToEnd가 돈다. 도우미(artex-sandbox)에 파일 cap이 있는 컨테이너에서
// 서버 uid로 실행할 때만 의미가 있다. 에이전트 명령은 도우미의 exec 모드로 에이전트 uid(ARTEX_AGENT_UID/GID)가
// 되어 돈다. server 패키지의 TestAgentSandboxEndToEnd와 같은 환경 변수를 쓴다.
const (
	sandboxE2EEnv       = "ARTEX_SANDBOX_E2E"
	sandboxE2EHelperEnv = "ARTEX_SANDBOX_E2E_HELPER"
)

// e2eRaceDuration 은 이름 바꾸기 경쟁을 돌리는 시간이다.
const e2eRaceDuration = 20 * time.Second

// TestSharedWorkspaceEndToEnd 는 uid 분리를 켠 실제 컨테이너에서 작업 공간 배치(sharedRunDirs)가 서버 uid 코드를
// 에이전트의 링크·바꿔치기에서 지키고, 작업·세션 uid끼리 작업 공간을 나누는지(#182) 본다. 결과 출력은 t.Log로 남겨
// PR의 컨테이너 확인 증거로 쓴다. 에이전트 명령은 풀의 uid(작업 A 20001, 작업 B 20002)와 같은 값의 gid로 돈다.
//
//   - 작업 A의 에이전트는 작업 B·다른 세션의 작업 공간·큰 출력·첨부와 drafts를 읽거나 쓰지 못한다.
//   - 에이전트는 tasks/<tid>와 큰 출력 디렉터리에 이름을 만들거나 서버가 만든 항목을 옮기지 못한다.
//   - fs.protected_regular=2인 호스트에서도 서버 uid의 Write·Edit가 에이전트가 만든 작업 공간 파일을 고친다.
//   - norma가 큰 출력을 쓸 때 에이전트가 미리 둔 링크를 따라가지 않는다.
//   - 파일 도구는 에이전트가 이름을 일반 파일과 키 링크로 계속 바꾸는 동안에도 키를 읽거나 쓰지 않는다.
func TestSharedWorkspaceEndToEnd(t *testing.T) {
	if os.Getenv(sandboxE2EEnv) != "1" {
		t.Skipf("%s=1 and a container with the sandbox helper are required", sandboxE2EEnv)
	}
	helper := os.Getenv(sandboxE2EHelperEnv)
	if helper == "" {
		helper = "/app/artex-sandbox"
	}
	const uidA, uidB = db.AgentUIDPoolFirst, db.AgentUIDPoolFirst + 1
	uid := strconv.Itoa(uidA)

	// 데이터 디렉터리와 그 아래 tasks·sessions·drafts는 server의 prepareWorkspaceRoots가 맞추는 권한과 같게 둔다.
	// t.TempDir()은 0700 상위 디렉터리 아래에 만들어 에이전트가 들어가지 못하므로 /tmp 바로 아래에 둔다.
	dataDir, err := os.MkdirTemp("", "artex-e2e-ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) }) // 정리 실패는 컨테이너가 사라지면 함께 없어진다
	for _, dir := range []string{dataDir, filepath.Join(dataDir, "tasks"), filepath.Join(dataDir, "sessions")} {
		if err := EnsureContainerDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	drafts := filepath.Join(dataDir, "drafts")
	if err := os.MkdirAll(filepath.Join(drafts, "d1", UploadsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(drafts, "d1", UploadsDirName, "a.txt"), "DRAFT-ATTACHMENT")
	if err := ShareDir(drafts, 0, WorkspaceServerOnly); err != nil {
		t.Fatal(err)
	}
	// 키는 서버 홈(0700) 아래에 둔다. 임시 디렉터리는 모든 역할의 파일 도구 쓰기 허용 경로라 그 아래 키는 경쟁 없이도
	// 닿는다(실제 키 디렉터리는 임시 디렉터리 밖이다).
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := os.MkdirTemp(home, "artex-e2e-keys-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(keys) })
	key := filepath.Join(keys, "jwt.key")
	const keyContent = "ORIGINAL-SERVER-KEY-165-0123456789abcdef"
	writeKey := func() {
		if err := os.WriteFile(key, []byte(keyContent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	assertKey := func(t *testing.T) {
		t.Helper()
		data, err := os.ReadFile(key)
		if err != nil || string(data) != keyContent {
			t.Fatalf("server key changed: %q, %v", data, err)
		}
	}
	writeKey()

	agentShAs := func(agentUID int, dir, cmd string) string {
		id := strconv.Itoa(agentUID)
		c := exec.Command(helper, "exec", "--", "sh", "-c", cmd)
		c.Dir = dir
		c.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "ARTEX_AGENT_UID=" + id, "ARTEX_AGENT_GID=" + id}
		out, err := c.CombinedOutput()
		return fmt.Sprintf("%s(exit: %v)", out, err)
	}
	agentSh := func(dir, cmd string) string { return agentShAs(uidA, dir, cmd) }
	planner, err := ensureRunDir(dataDir, 7, 0, uidA)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := ensureRunDir(dataDir, 7, 1, uidA)
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := ensureRunDir(dataDir, 8, 2, uidB)
	if err != nil {
		t.Fatal(err)
	}
	sessionA, err := ensureSessionDir(filepath.Join(dataDir, "sessions", "conv-1"), uidA)
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := ensureSessionDir(filepath.Join(dataDir, "sessions", "conv-2"), uidB)
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Dir(planner.work)
	if out, err := exec.Command("ls", "-laR", taskDir).CombinedOutput(); err == nil {
		t.Logf("task dir:\n%s", out)
	}
	if data, err := os.ReadFile("/proc/sys/fs/protected_regular"); err == nil {
		t.Logf("fs.protected_regular = %s", strings.TrimSpace(string(data)))
	}
	t.Logf("agent search tools: %s", agentSh(planner.work, "id; for c in rg grep find ls; do command -v $c || echo \"$c: not found\"; done"))
	if out, err := exec.Command("getfacl", "-n", "-p", filepath.Dir(planner.work), planner.work, planner.toolOutput, sessionA.work).CombinedOutput(); err == nil {
		t.Logf("getfacl:\n%s", out)
	} else {
		t.Logf("getfacl is not available: %v: %s", err, out)
	}

	t.Run("tasks and sessions cannot reach each other", func(t *testing.T) {
		for _, f := range []struct{ dir, name, content string }{
			{otherTask.work, "secret.txt", "TASK-B-SECRET"},
			{sessionB.work, "secret.txt", "SESSION-B-SECRET"},
		} {
			if out := agentShAs(uidB, f.dir, "echo "+f.content+" > "+f.name+" && echo written"); !strings.Contains(out, "written") {
				t.Fatalf("owner could not write its own workspace: %q", out)
			}
		}
		mustWrite(t, filepath.Join(otherTask.toolOutput, "output-1-1.txt"), "TASK-B-OUTPUT")
		if err := os.MkdirAll(filepath.Join(filepath.Dir(otherTask.work), UploadsDirName), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(filepath.Dir(otherTask.work), UploadsDirName, "b.txt"), "TASK-B-UPLOAD")
		targets := []string{
			otherTask.work, filepath.Join(otherTask.work, "secret.txt"), otherTask.toolOutput,
			filepath.Join(otherTask.toolOutput, "output-1-1.txt"), filepath.Join(filepath.Dir(otherTask.work), UploadsDirName, "b.txt"),
			sessionB.work, filepath.Join(sessionB.work, "secret.txt"), sessionB.toolOutput,
			filepath.Join(dataDir, "tasks"), filepath.Join(dataDir, "sessions"), drafts, filepath.Join(drafts, "d1", UploadsDirName, "a.txt"),
		}
		var cmd strings.Builder
		for _, p := range targets {
			fmt.Fprintf(&cmd, "echo '## %s'; ls %s 2>&1 | head -3; cat %s 2>&1 | head -1; touch %s/x 2>&1; ", p, p, p, p)
		}
		for _, run := range []struct {
			name string
			dir  string
		}{{"task A", planner.work}, {"session A", sessionA.work}} {
			out := agentSh(run.dir, cmd.String())
			t.Logf("%s (uid %d) reaches for task B (uid %d), session B and drafts:\n%s", run.name, uidA, uidB, out)
			// 대상마다 ls·cat·touch가 모두 권한 오류여야 하고, B의 내용이 하나도 보이면 안 된다.
			for _, part := range strings.Split(out, "## ")[1:] {
				if strings.Count(part, "Permission denied") < 3 {
					t.Fatalf("%s was not denied every access: %q", run.name, part)
				}
			}
			for _, secret := range []string{"TASK-B-SECRET", "SESSION-B-SECRET", "TASK-B-OUTPUT", "TASK-B-UPLOAD", "DRAFT-ATTACHMENT"} {
				if strings.Contains(out, secret) {
					t.Fatalf("%s read %q from another task, session or drafts", run.name, secret)
				}
			}
			for _, p := range targets {
				if _, err := os.Stat(filepath.Join(p, "x")); err == nil {
					t.Fatalf("%s created a file in %s", run.name, p)
				}
			}
		}
		// 서버 uid의 파일 도구 Read는 drafts의 첨부를 읽는다(작업 생성 전 첨부는 이 길로만 읽는다).
		tools := e2eFileTools(fileScopeFor(fileRoleTask, fileScopeInput{DataDir: dataDir, TaskID: 7}).forSharing(true))
		res := callTool(t, tools["Read"], &actool.ToolContext{WorkingDir: planner.work}, map[string]any{"file_path": filepath.Join(drafts, "d1", UploadsDirName, "a.txt")})
		t.Logf("server Read of the draft attachment: isError=%v %q", res.IsError, res.Flatten())
		if res.IsError || !strings.Contains(res.Flatten(), "DRAFT-ATTACHMENT") {
			t.Fatalf("file tool Read could not read the draft attachment: %q", res.Flatten())
		}
	})

	t.Run("agent cannot replace server entries", func(t *testing.T) {
		cmd := strings.Join([]string{
			"mv ../i1 ../moved-i1", "mv ../work ../moved-work", "ln -s " + keys + " ../i2", "mkdir ../i3", "touch ../planted",
			"mv ../cmd-output ../moved-out", "ln -s " + key + " ../cmd-output/work/output-1-1.txt", "touch ../cmd-output/i1/planted",
			"echo done",
		}, "; ")
		out := agentSh(planner.work, cmd)
		t.Log(out)
		if got := strings.Count(out, "Permission denied"); got < 8 {
			t.Fatalf("expected every attempt to be denied, got %d denials: %q", got, out)
		}
		for _, rel := range []string{"i1", "work", "cmd-output", "cmd-output/work", "cmd-output/i1"} {
			if info, err := os.Lstat(filepath.Join(taskDir, rel)); err != nil || !info.IsDir() {
				t.Fatalf("%s was replaced: %v, %v", rel, info, err)
			}
		}
		for _, rel := range []string{"moved-i1", "moved-work", "i2", "i3", "planted", "moved-out", "cmd-output/work/output-1-1.txt", "cmd-output/i1/planted"} {
			if _, err := os.Lstat(filepath.Join(taskDir, rel)); err == nil {
				t.Fatalf("agent created %s", rel)
			}
		}
	})

	t.Run("server file tools rewrite agent files", func(t *testing.T) {
		for _, run := range []struct {
			name string
			role fileRole
			in   fileScopeInput
			dir  string
		}{
			{"planner", fileRoleTask, fileScopeInput{DataDir: dataDir, TaskID: 7}, planner.work},
			{"worker", fileRoleIntent, fileScopeInput{DataDir: dataDir, TaskID: 7, IntentID: 1}, worker.work},
		} {
			out := agentSh(run.dir, "echo agent > agent.txt && ls -ln agent.txt")
			t.Logf("%s: agent made a file: %s", run.name, out)
			tools := e2eFileTools(fileScopeFor(run.role, run.in).forSharing(true))
			tc := &actool.ToolContext{WorkingDir: run.dir}
			for _, call := range []struct {
				tool  string
				input map[string]any
			}{
				{"Write", map[string]any{"file_path": "agent.txt", "content": "server wrote\n"}},
				{"Edit", map[string]any{"file_path": "agent.txt", "old_string": "wrote", "new_string": "edited"}},
				{"MultiEdit", map[string]any{"file_path": "agent.txt", "edits": []map[string]any{{"old_string": "server", "new_string": "SERVER"}}}},
			} {
				res := callTool(t, tools[call.tool], tc, call.input)
				t.Logf("%s: %s on the agent's file -> isError=%v %q", run.name, call.tool, res.IsError, res.Flatten())
				if res.IsError {
					t.Fatalf("%s %s failed on the agent's file", run.name, call.tool)
				}
			}
			out = agentSh(run.dir, "cat agent.txt && echo agent again >> agent.txt && echo appended")
			t.Logf("%s: agent reads it back: %s", run.name, out)
			if !strings.Contains(out, "SERVER edited") || !strings.Contains(out, "appended") {
				t.Fatalf("%s: agent could not read or append the server's edit: %q", run.name, out)
			}
		}
	})

	t.Run("large output spill ignores planted links", func(t *testing.T) {
		writeKey()
		now := time.Now().Unix()
		// 예전 위치(작업 디렉터리 안 cmd-output)와 지금 위치(서버 전용) 모두에 다음 출력 파일 이름으로 키 링크를 깐다.
		plant := fmt.Sprintf("for d in cmd-output %s; do mkdir -p $d; n=0; for s in $(seq %d %d); do for i in $(seq 1 50); do ln -s %s $d/output-$s-$i.txt 2>/dev/null && n=$((n+1)); done; done; echo \"$d: planted $n\"; done",
			planner.toolOutput, now, now+30, key)
		t.Logf("agent plants links: %s", agentSh(planner.work, plant))
		big := strings.Repeat("AGENT-CHOSEN-CONTENT-0123456789\n", 2000)
		out := actool.Capture(&actool.ToolContext{WorkingDir: planner.work, OutputDir: planner.toolOutput}, big)
		tail := out[strings.LastIndex(out, "<persisted-output>"):]
		t.Logf("capture tail: %s", tail)
		assertKey(t)
		spills, _ := filepath.Glob(filepath.Join(planner.toolOutput, "output-*.txt"))
		if len(spills) != 1 {
			t.Fatalf("spill files = %v, want one", spills)
		}
		info, err := os.Lstat(spills[0])
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("spill file is not a regular file: %v, %v", info, err)
		}
		readBack := agentSh(planner.work, "head -c 64 "+spills[0])
		t.Logf("agent reads the spill: %s", readBack)
		if !strings.Contains(readBack, "AGENT-CHOSEN-CONTENT") {
			t.Fatalf("agent cannot read the spill file: %q", readBack)
		}
		tools := e2eFileTools(fileScopeFor(fileRoleTask, fileScopeInput{DataDir: dataDir, TaskID: 7}).forSharing(true))
		res := callTool(t, tools["Read"], &actool.ToolContext{WorkingDir: planner.work}, map[string]any{"file_path": spills[0], "limit": 1})
		if res.IsError || !strings.Contains(res.Flatten(), "AGENT-CHOSEN-CONTENT") {
			t.Fatalf("Read of the spill file: %q", res.Flatten())
		}
	})

	t.Run("file tools survive the swap race", func(t *testing.T) {
		writeKey()
		// 에이전트가 같은 이름을 일반 파일과 키 링크로 계속 바꾼다(rename은 원자적이다). timeout으로 스스로 끝난다.
		loop := fmt.Sprintf("timeout %d sh -c 'while :; do printf \"FAKE notes\\n\" > .r; mv -f .r notes.txt; ln -sf %s .l; mv -f .l notes.txt; done'",
			int((e2eRaceDuration + 2*time.Second).Seconds()), key)
		c := exec.Command(helper, "exec", "--", "sh", "-c", loop)
		c.Dir = worker.work
		c.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "ARTEX_AGENT_UID=" + uid, "ARTEX_AGENT_GID=" + uid}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		// timeout이 끝내며 0이 아닌 종료 코드를 내므로 결과는 버린다. 기다리기만 해 프로세스를 거둔다.
		defer func() { _ = c.Wait() }()
		tools := e2eFileTools(fileScopeFor(fileRoleIntent, fileScopeInput{DataDir: dataDir, TaskID: 7, IntentID: 1}).forSharing(true))
		tc := &actool.ToolContext{WorkingDir: worker.work}
		calls, refused := 0, 0
		deadline := time.Now().Add(e2eRaceDuration)
		for time.Now().Before(deadline) {
			for _, call := range []struct {
				tool  string
				input map[string]any
			}{
				{"Read", map[string]any{"file_path": "notes.txt"}},
				{"Write", map[string]any{"file_path": "notes.txt", "content": "AGENT-CHOSEN"}},
				{"Edit", map[string]any{"file_path": "notes.txt", "old_string": "FAKE", "new_string": "AGENT"}},
			} {
				res := callTool(t, tools[call.tool], tc, call.input)
				calls++
				if res.IsError {
					refused++
				}
				if strings.Contains(res.Flatten(), keyContent) {
					t.Fatalf("after %d calls: %s returned the key through a swapped link: %q", calls, call.tool, res.Flatten())
				}
			}
		}
		t.Logf("race: %d file tool calls in %s, %d refused or failed (the name was a link at that moment), key untouched", calls, e2eRaceDuration, refused)
		assertKey(t)
		if refused == 0 {
			t.Fatal("the agent loop never swapped the name; the race did not run")
		}
	})
}

// e2eFileTools 는 scope로 감싼 파일 도구를 이름으로 찾게 돌려준다.
func e2eFileTools(scope FileScope) map[string]actool.CoreTool {
	tools := map[string]actool.CoreTool{}
	for _, tool := range confineFileTools(actool.DefaultTools(), scope) {
		tools[tool.Name()] = tool
	}
	return tools
}
