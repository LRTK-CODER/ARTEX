package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
)

// TestDeleteTaskKeepingFilesClosesWorkspaceBeforeReturningUID 는 uid 분리를 켰을 때 파일을 남기고(화면의 기본값)
// 작업을 지우면 tasks/<id>를 서버 전용으로 닫은 뒤에 uid를 풀로 돌려주는지 본다(#182). 닫지 않으면 같은 uid를 다시
// 받은 작업·세션이 지운 작업의 작업 공간·첨부·큰 출력을 읽고 쓴다. 닫지 못하면 작업을 지우지 않아 uid도 돌아가지 않는다.
func TestDeleteTaskKeepingFilesClosesWorkspaceBeforeReturningUID(t *testing.T) {
	skipWithoutACL(t)
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	s := newTestServer(t, m, t.TempDir(), t.TempDir(), t.TempDir())
	s.sandbox = &AgentSandbox{uid: 20000, gid: 20000}
	if err := prepareWorkspaceRoots(m.dir); err != nil {
		t.Fatal(err)
	}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// newTask 는 uid를 받은 작업과, 실행이 거는 것처럼 그 uid에 열린 tasks/<id>(읽기)·work(읽기·쓰기)를 만든다.
	newTask := func(t *testing.T, title string) (string, int, string) {
		t.Helper()
		task, err := m.CreateTask(title, "", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(task.ID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
		uid, err := m.pg.AcquireTaskAgentUID(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		taskDir := filepath.Join(m.dir, "tasks", task.ID)
		work := filepath.Join(taskDir, "work")
		for _, step := range []struct {
			dir    string
			access agent.WorkspaceAccess
		}{{taskDir, agent.WorkspaceRead}, {work, agent.WorkspaceWrite}} {
			if err := os.MkdirAll(step.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := agent.ShareDir(step.dir, uid, step.access); err != nil {
				t.Fatal(err)
			}
		}
		return task.ID, uid, taskDir
	}
	deleteTask := func(t *testing.T, id string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/tasks/"+id, strings.NewReader(`{"delete_files":false}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	uidAssigned := func(t *testing.T, uid int) bool {
		t.Helper()
		var n int
		if err := m.pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE uid=$1`, uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	taskExists := func(t *testing.T, id string) bool {
		t.Helper()
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		got, err := m.pg.GetTask(n)
		if err != nil {
			t.Fatal(err)
		}
		return got != nil
	}

	t.Run("closes the workspace, then returns the uid", func(t *testing.T) {
		id, uid, taskDir := newTask(t, "close then return")
		rec := deleteTask(t, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("delete status = %d %s", rec.Code, rec.Body.String())
		}
		info, err := os.Stat(taskDir)
		if err != nil {
			t.Fatalf("task files were not kept: %v", err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want 0700", taskDir, info.Mode().Perm())
		}
		if acl := readACLOf(t, taskDir, agent.ACLAccess); acl != nil {
			t.Errorf("%s still has an access ACL %v naming the returned uid", taskDir, acl)
		}
		if taskExists(t, id) {
			t.Fatal("task was not deleted")
		}
		if uidAssigned(t, uid) {
			t.Fatalf("uid %d still assigned after the task was deleted", uid)
		}
	})

	t.Run("keeps the task and its uid when the workspace cannot be closed", func(t *testing.T) {
		id, uid, taskDir := newTask(t, "cannot close")
		// 작업 디렉터리 자리에 링크가 있으면 따라가지 않고 닫기를 거부한다.
		if err := os.RemoveAll(taskDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), taskDir); err != nil {
			t.Fatal(err)
		}
		rec := deleteTask(t, id)
		if rec.Code != http.StatusConflict {
			t.Fatalf("delete status = %d %s, want 409", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "작업 공간을 닫지 못해 지우지 않았습니다") {
			t.Errorf("409 body = %s", rec.Body.String())
		}
		if !taskExists(t, id) {
			t.Fatal("task was deleted although its workspace stayed open")
		}
		if !uidAssigned(t, uid) {
			t.Fatalf("uid %d was returned although the workspace stayed open", uid)
		}
	})
}
