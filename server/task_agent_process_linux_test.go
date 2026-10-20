package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

// TestTaskDeleteAndArchiveStopAgentProcessesFirst 는 uid 분리를 켰을 때 작업을 지우거나(파일을 지우든 남기든) 보관하기
// 전에 그 작업 uid의 남은 프로세스를 끝내고, 끝내지 못하면 지우기·보관을 멈추는지 본다(#179). 셸에서 떼어 띄운 프로세스는
// 실행이 끝나도 남아 cwd·열린 fd로 작업 공간에 닿고, 작업을 지워 uid가 다른 작업에 다시 할당되면 새 작업 공간에도 닿는다.
// 보관한 작업을 복원하면 작업 공간이 다시 그 작업 uid에 열린다. 실제로 프로세스를 끝내는 확인은 컨테이너 e2e에 있다
// (실제 구현은 그 uid가 되어 kill(-1)을 보내므로 여기서 돌리지 않는다).
func TestTaskDeleteAndArchiveStopAgentProcessesFirst(t *testing.T) {
	skipWithoutACL(t)
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	s := newTestServerWithoutArchiveWorker(t, m, t.TempDir(), t.TempDir(), t.TempDir())
	s.sandbox = &AgentSandbox{uid: 20000, gid: 20000}
	m.sandbox = s.sandbox
	if err := prepareWorkspaceRoots(m.dir); err != nil {
		t.Fatal(err)
	}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// stopCall 은 가짜 stopAgentUID가 불린 시점의 상태다. 작업 공간이 아직 제자리에 있었는지, 그 uid에 열려 있었는지,
	// uid가 아직 할당돼 있었는지 남긴다.
	type stopCall struct {
		uid              int
		workspaceInPlace bool
		workspaceOpen    bool
		uidStillAssigned bool
	}
	var stops []stopCall
	var stopErr error
	taskDirs := map[int]string{}
	s.stopAgentUID = func(_ context.Context, uid int) error {
		var n int
		if err := m.pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE uid=$1`, uid).Scan(&n); err != nil {
			return err
		}
		call := stopCall{uid: uid, uidStillAssigned: n > 0}
		if dir, ok := taskDirs[uid]; ok {
			if f, err := os.Open(dir); err == nil {
				call.workspaceInPlace = true
				acl, _ := agent.GetACL(f, agent.ACLAccess) // ACL이 없으면(닫힘) 빈 값이다
				f.Close()
				call.workspaceOpen = slices.ContainsFunc(acl, func(e agent.ACLEntry) bool { return e.Tag == agent.ACLNamedUser && int(e.ID) == uid })
			}
		}
		stops = append(stops, call)
		return stopErr
	}

	// newTask 는 uid를 받은 작업과, 실행이 거는 것처럼 그 uid에 열린 작업 공간(tasks/<id>, work/notes.md)을 만든다.
	newTask := func(t *testing.T, name string) (string, int64, int) {
		t.Helper()
		task, taskID := newAgentUIDTestTask(t, m, name)
		uid, err := m.pg.AcquireTaskAgentUID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		taskDir := filepath.Join(m.dir, "tasks", task.ID)
		work := filepath.Join(taskDir, "work")
		for dir, access := range map[string]agent.WorkspaceAccess{taskDir: agent.WorkspaceRead, work: agent.WorkspaceWrite} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := agent.ShareDir(dir, uid, access); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("task notes"), 0o600); err != nil {
			t.Fatal(err)
		}
		taskDirs[uid] = taskDir
		return task.ID, taskID, uid
	}
	uidAssigned := func(t *testing.T, uid int) bool {
		t.Helper()
		var n int
		if err := m.pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE uid=$1`, uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	deleteTask := func(t *testing.T, id string, deleteFiles bool) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"delete_files":` + strconv.FormatBool(deleteFiles) + `}`
		req := httptest.NewRequest(http.MethodDelete, "/api/tasks/"+id, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		name        string
		deleteFiles bool
		// 파일을 남기는 삭제는 작업 공간을 닫은 뒤에 끝낸다. 파일을 지우는 삭제는 작업 공간을 옮기기 전에 끝낸다.
		wantOpenAtStop bool
	}{
		{name: "delete with files", deleteFiles: true, wantOpenAtStop: true},
		{name: "delete keeping files", deleteFiles: false, wantOpenAtStop: false},
	} {
		t.Run(tc.name+" stops the task's processes before returning its uid", func(t *testing.T) {
			stops, stopErr = nil, nil
			id, _, uid := newTask(t, tc.name)
			rec := deleteTask(t, id, tc.deleteFiles)
			if rec.Code != http.StatusOK {
				t.Fatalf("delete status = %d %s", rec.Code, rec.Body.String())
			}
			want := []stopCall{{uid: uid, workspaceInPlace: true, workspaceOpen: tc.wantOpenAtStop, uidStillAssigned: true}}
			if !slices.Equal(stops, want) {
				t.Fatalf("agent process stops = %+v, want %+v", stops, want)
			}
			if uidAssigned(t, uid) {
				t.Fatalf("uid %d still assigned after the task was deleted", uid)
			}
			_, err := os.Lstat(filepath.Join(m.dir, "tasks", id))
			if gone := os.IsNotExist(err); gone != tc.deleteFiles {
				t.Fatalf("task files gone = %v (%v), want %v", gone, err, tc.deleteFiles)
			}
		})
		t.Run(tc.name+" keeps the task when its processes cannot be stopped", func(t *testing.T) {
			stops, stopErr = nil, errAgentProcessesRemain
			t.Cleanup(func() { stopErr = nil })
			id, taskID, uid := newTask(t, tc.name+" refused")
			rec := deleteTask(t, id, tc.deleteFiles)
			if rec.Code != http.StatusConflict {
				t.Fatalf("delete status = %d %s, want 409", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, "에이전트 프로세스를 끝내지 못해 지우지 않았습니다") {
				t.Fatalf("delete body = %s, want the agent process message", body)
			}
			if task, err := m.pg.GetTask(taskID); err != nil || task == nil {
				t.Fatalf("task was deleted: %+v, %v", task, err)
			}
			if !uidAssigned(t, uid) {
				t.Fatalf("uid %d was returned although its processes were not stopped", uid)
			}
			if got, err := os.ReadFile(filepath.Join(m.dir, "tasks", id, "work", "notes.md")); err != nil || string(got) != "task notes" {
				t.Fatalf("task files moved although the delete was refused: %q, %v", got, err)
			}
		})
	}

	t.Run("archive is refused while the task's processes cannot be stopped", func(t *testing.T) {
		stops, stopErr = nil, errAgentProcessesRemain
		t.Cleanup(func() { stopErr = nil })
		id, taskID, uid := newTask(t, "archive refused")
		if err := m.SetTaskPaused(id, true); err != nil {
			t.Fatal(err)
		}
		job, err := m.pg.QueueTaskArchive(taskID)
		if err != nil {
			t.Fatal(err)
		}
		err = s.runOneTaskArchiveJob()
		if err == nil || !strings.Contains(err.Error(), "에이전트 프로세스를 끝내지 못해 보관하지 않았습니다") {
			t.Fatalf("archive error = %v, want the agent process message", err)
		}
		if want := []stopCall{{uid: uid, workspaceInPlace: true, workspaceOpen: true, uidStillAssigned: true}}; !slices.Equal(stops, want) {
			t.Fatalf("agent process stops = %+v, want %+v", stops, want)
		}
		if got, err := os.ReadFile(filepath.Join(m.dir, "tasks", id, "work", "notes.md")); err != nil || string(got) != "task notes" {
			t.Fatalf("task files moved although the archive was refused: %q, %v", got, err)
		}
		if got, err := m.pg.GetTaskArchive(job.ID); err != nil || got == nil || got.State == db.ArchiveReady {
			t.Fatalf("archive job = %+v, %v; want it not ready", got, err)
		}
	})

	// 보관 → 복원: 보관 전에 프로세스를 끝내고, 복원하면 풀린 작업 공간(모두 서버 소유, ACL 없음)을 같은 작업 uid에 다시
	// 연다(restoreTaskArchivePayload → reopenRestoredWorkspace).
	t.Run("archive stops the processes and restore reopens the workspace to the task uid", func(t *testing.T) {
		stops, stopErr = nil, nil
		id, taskID, uid := newTask(t, "archive restore")
		if err := m.SetTaskPaused(id, true); err != nil {
			t.Fatal(err)
		}
		job, err := m.pg.QueueTaskArchive(taskID)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.runOneTaskArchiveJob(); err != nil {
			t.Fatal(err)
		}
		if want := []stopCall{{uid: uid, workspaceInPlace: true, workspaceOpen: true, uidStillAssigned: true}}; !slices.Equal(stops, want) {
			t.Fatalf("agent process stops = %+v, want %+v", stops, want)
		}
		if _, err := os.Lstat(filepath.Join(m.dir, "tasks", id)); !os.IsNotExist(err) {
			t.Fatalf("task files still hot after archive: %v", err)
		}
		if _, err := m.pg.QueueTaskArchiveRestore(job.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.runOneTaskArchiveJob(); err != nil {
			t.Fatal(err)
		}
		work := filepath.Join(m.dir, "tasks", id, "work")
		if got, err := os.ReadFile(filepath.Join(work, "notes.md")); err != nil || string(got) != "task notes" {
			t.Fatalf("restored notes = %q, %v", got, err)
		}
		for _, path := range []string{filepath.Join(m.dir, "tasks", id), work, filepath.Join(work, "notes.md")} {
			acl := readACLOf(t, path, agent.ACLAccess)
			if !slices.ContainsFunc(acl, func(e agent.ACLEntry) bool { return e.Tag == agent.ACLNamedUser && int(e.ID) == uid }) {
				t.Errorf("%s ACL %v does not open the restored workspace to task uid %d", path, acl, uid)
			}
		}
	})
}
