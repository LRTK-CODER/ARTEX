package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

const fakeKeyContent = "FAKE-OAUTH-KEY-169"

// fileScopeFixture 는 작업 공간·읽기 전용 디렉터리·밖(키 디렉터리)을 t.TempDir() 아래에 만든다.
type fileScopeFixture struct {
	workspace, readOnly, outside, keyPath string
	tools                                 map[string]actool.CoreTool
	tc                                    *actool.ToolContext
}

func newFileScopeFixture(t *testing.T) fileScopeFixture {
	t.Helper()
	base := t.TempDir()
	f := fileScopeFixture{
		workspace: filepath.Join(base, "data", "sessions", "conv-1"),
		readOnly:  filepath.Join(base, "data", "tasks"),
		outside:   filepath.Join(base, "keys"),
	}
	for _, d := range []string{f.workspace, f.readOnly, f.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.keyPath = filepath.Join(f.outside, "oauth.key")
	mustWrite(t, f.keyPath, fakeKeyContent)
	mustWrite(t, filepath.Join(f.workspace, "notes.txt"), "hello workspace\n")
	mustWrite(t, filepath.Join(f.readOnly, "report.txt"), "task output\n")
	// 작업 공간 안에서 밖의 키를 가리키는 링크와, 밖의 없는 파일을 가리키는 끊긴 링크.
	if err := os.Symlink(f.keyPath, filepath.Join(f.workspace, "key-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.outside, "planted"), filepath.Join(f.workspace, "dangling")); err != nil {
		t.Fatal(err)
	}

	// 임시 디렉터리 허용이 t.TempDir() 전체를 열지 않게, 표 대신 직접 만든 범위로 감싼다.
	scope := FileScope{Writable: []string{f.workspace}, Readable: []string{f.readOnly}}
	ctx := withFileScope(context.Background(), scope)
	tools, _, cleanup := AugmentTools(ctx, "chat", actool.DefaultTools())
	t.Cleanup(cleanup)
	f.tools = map[string]actool.CoreTool{}
	for _, tool := range tools {
		f.tools[tool.Name()] = tool
	}
	f.tc = &actool.ToolContext{WorkingDir: f.workspace}
	return f
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func callTool(t *testing.T, tool actool.CoreTool, tc *actool.ToolContext, input map[string]any) actool.Result {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(context.Background(), raw, tc)
	if err != nil {
		t.Fatalf("%s returned error: %v", tool.Name(), err)
	}
	return res
}

func TestFileToolsRejectPathsOutsideScope(t *testing.T) {
	f := newFileScopeFixture(t)
	cases := []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{"absolute key path", "Read", map[string]any{"file_path": f.keyPath}},
		{"dot-dot escape", "Read", map[string]any{"file_path": "../../../keys/oauth.key"}},
		{"link inside workspace to key", "Read", map[string]any{"file_path": "key-link"}},
		{"key name with different case", "Read", map[string]any{"FILE_PATH": f.keyPath}},
		{"write new file outside", "Write", map[string]any{"file_path": filepath.Join(f.outside, "new.txt"), "content": "x"}},
		{"write through link to key", "Write", map[string]any{"file_path": "key-link", "content": "x"}},
		{"write through dangling link", "Write", map[string]any{"file_path": "dangling", "content": "x"}},
		{"write into read-only dir", "Write", map[string]any{"file_path": filepath.Join(f.readOnly, "x.txt"), "content": "x"}},
		{"edit key", "Edit", map[string]any{"file_path": f.keyPath, "old_string": "FAKE", "new_string": "REAL"}},
		{"multiedit key", "MultiEdit", map[string]any{"file_path": f.keyPath, "edits": []map[string]any{{"old_string": "FAKE", "new_string": "REAL"}}}},
		{"list outside dir", "LS", map[string]any{"path": f.outside}},
		{"glob outside dir", "Glob", map[string]any{"pattern": "*", "path": f.outside}},
		{"grep outside dir", "Grep", map[string]any{"pattern": "FAKE", "path": f.outside, "output_mode": "content"}},
	}
	realKey, err := filepath.EvalSymlinks(f.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, f.tools[tc.tool], f.tc, tc.input)
			out := res.Flatten()
			if !res.IsError || !strings.Contains(out, "refused") {
				t.Fatalf("want refusal, got IsError=%v %q", res.IsError, out)
			}
			if strings.Contains(out, fakeKeyContent) {
				t.Fatalf("refusal leaks key content: %q", out)
			}
			// 입력에 없던 실제 경로(링크를 푼 키 경로)를 메시지에 싣지 않는다.
			inputJSON, _ := json.Marshal(tc.input)
			for _, real := range []string{f.keyPath, realKey} {
				if !strings.Contains(string(inputJSON), real) && strings.Contains(out, real) {
					t.Fatalf("refusal names the resolved real path: %q", out)
				}
			}
		})
	}
	data, err := os.ReadFile(f.keyPath)
	if err != nil || string(data) != fakeKeyContent {
		t.Fatalf("key file changed: %q, %v", data, err)
	}
	for _, p := range []string{filepath.Join(f.outside, "new.txt"), filepath.Join(f.outside, "planted"), filepath.Join(f.readOnly, "x.txt")} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("file created outside writable scope: %s", p)
		}
	}
}

func TestFileToolsAllowPathsInsideScope(t *testing.T) {
	f := newFileScopeFixture(t)

	if res := callTool(t, f.tools["Read"], f.tc, map[string]any{"file_path": "notes.txt"}); res.IsError || !strings.Contains(res.Flatten(), "hello workspace") {
		t.Fatalf("read inside workspace: %q", res.Flatten())
	}
	if res := callTool(t, f.tools["Read"], f.tc, map[string]any{"file_path": filepath.Join(f.readOnly, "report.txt")}); res.IsError || !strings.Contains(res.Flatten(), "task output") {
		t.Fatalf("read inside read-only dir: %q", res.Flatten())
	}
	newFile := filepath.Join("sub", "dir", "out.txt") // 부모 디렉터리도 아직 없다
	if res := callTool(t, f.tools["Write"], f.tc, map[string]any{"file_path": newFile, "content": "written"}); res.IsError {
		t.Fatalf("write new file inside workspace: %q", res.Flatten())
	}
	if data, err := os.ReadFile(filepath.Join(f.workspace, newFile)); err != nil || string(data) != "written" {
		t.Fatalf("written file: %q, %v", data, err)
	}
	if res := callTool(t, f.tools["Edit"], f.tc, map[string]any{"file_path": newFile, "old_string": "written", "new_string": "edited"}); res.IsError {
		t.Fatalf("edit inside workspace: %q", res.Flatten())
	}
	if res := callTool(t, f.tools["LS"], f.tc, map[string]any{}); res.IsError || !strings.Contains(res.Flatten(), "notes.txt") {
		t.Fatalf("ls of working dir: %q", res.Flatten())
	}
	if res := callTool(t, f.tools["Glob"], f.tc, map[string]any{"pattern": "**/*.txt"}); res.IsError || !strings.Contains(res.Flatten(), "out.txt") {
		t.Fatalf("glob in working dir: %q", res.Flatten())
	}
}

func TestGrepRejectsTreeWithLinkOutsideScope(t *testing.T) {
	// 순수 Go 대체 구현은 트리 안 파일 링크를 따라 읽는다. ripgrep이 없을 때의 경로를 고정해 확인한다.
	t.Setenv("NORMA_DISABLE_RIPGREP", "1")
	f := newFileScopeFixture(t)

	res := callTool(t, f.tools["Grep"], f.tc, map[string]any{"pattern": "FAKE", "output_mode": "content"})
	if !res.IsError || !strings.Contains(res.Flatten(), "key-link") || strings.Contains(res.Flatten(), fakeKeyContent) {
		t.Fatalf("want refusal naming the link without content, got %q", res.Flatten())
	}

	if err := os.Remove(filepath.Join(f.workspace, "key-link")); err != nil {
		t.Fatal(err)
	}
	res = callTool(t, f.tools["Grep"], f.tc, map[string]any{"pattern": "hello", "output_mode": "content"})
	if res.IsError || !strings.Contains(res.Flatten(), "hello workspace") {
		t.Fatalf("grep inside workspace: %q", res.Flatten())
	}
}

// TestFileToolsWithoutScopeRejectEverything 은 허용 경로를 싣지 않은 실행이 파일 도구로 아무것도 읽지
// 못하는지 본다. 고치기 전에는 norma 도구가 그대로 붙어 키를 읽었다.
func TestFileToolsWithoutScopeRejectEverything(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "oauth.key")
	mustWrite(t, key, fakeKeyContent)
	tools, _, cleanup := AugmentTools(context.Background(), "chat", actool.DefaultTools())
	defer cleanup()
	for _, tool := range tools {
		if tool.Name() != "Read" {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"file_path": key})
		res, err := tool.Call(context.Background(), raw, &actool.ToolContext{WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || strings.Contains(res.Flatten(), fakeKeyContent) {
			t.Fatalf("Read without scope: IsError=%v %q", res.IsError, res.Flatten())
		}
		return
	}
	t.Fatal("Read tool missing")
}

func TestFileScopeForRoles(t *testing.T) {
	data := filepath.Join(string(filepath.Separator), "srv", "artex")
	tmp := os.TempDir()
	task := func(id string) string { return filepath.Join(data, "tasks", id) }
	drafts := filepath.Join(data, "drafts")
	cases := []struct {
		name string
		role fileRole
		in   fileScopeInput
		want FileScope
	}{
		{"planner and mainagent", fileRoleTask, fileScopeInput{DataDir: data, TaskID: 7, SourceTaskIDs: []int64{3}},
			FileScope{Writable: []string{task("7"), tmp}, Readable: []string{drafts, task("3")}}},
		{"worker", fileRoleIntent, fileScopeInput{DataDir: data, TaskID: 7, IntentID: 42, SourceTaskIDs: []int64{3}},
			FileScope{Writable: []string{filepath.Join(task("7"), "i42"), tmp}, Readable: []string{task("7"), drafts, task("3")}}},
		{"conversation", fileRoleConversation, fileScopeInput{DataDir: data, SessionDir: filepath.Join(data, "sessions", "conv-9")},
			FileScope{Writable: []string{filepath.Join(data, "sessions", "conv-9"), tmp}, Readable: []string{filepath.Join(data, "tasks"), drafts}}},
		{"unknown role", fileRole("other"), fileScopeInput{DataDir: data}, FileScope{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fileScopeFor(tc.role, tc.in)
			if strings.Join(got.Writable, "|") != strings.Join(tc.want.Writable, "|") || strings.Join(got.Readable, "|") != strings.Join(tc.want.Readable, "|") {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
