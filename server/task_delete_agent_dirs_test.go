package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/config"
)

// TestDeleteTaskClearsAgentDirsBeforeReturningUID 는 uid 분리를 켰을 때 파일까지 지우는 작업 삭제가, 에이전트가 권한을
// 바꾼 디렉터리를 도우미로 비우는 단계를 uid를 풀로 돌려주기(DB 삭제) 전에 끝내는지 본다(#179). 돌려준 뒤에 그 uid로
// 도우미를 띄우면 같은 uid를 바로 받은 작업의 프로세스가 도우미의 cwd·fd를 거쳐 지우는 작업의 파일에 닿는다. 비우지
// 못하면 작업을 지우지 않아 uid도 돌아가지 않는다. 실제 도우미로 비우는 확인은 컨테이너 e2e에 있다.
func TestDeleteTaskClearsAgentDirsBeforeReturningUID(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	s := newTestServer(t, m, t.TempDir(), t.TempDir(), t.TempDir())
	s.sandbox = &AgentSandbox{uid: 20000, gid: 20000}
	m.sandbox = s.sandbox
	// 남은 프로세스 끝내기는 TestTaskDeleteAndArchiveStopAgentProcessesFirst가 본다. 실제 구현은 그 uid가 되어
	// kill(-1)을 보내므로 여기서는 끝낸 것으로 둔다.
	s.stopAgentUID = func(context.Context, int) error { return nil }
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// clearCall 은 가짜 clearAgentDirs가 불린 시점의 상태다.
	type clearCall struct {
		inDeleteStaging  bool // 서버 전용 임시 자리(.delete-staging) 아래를 비운다
		workspaceMoved   bool // 작업 공간은 이미 제자리(tasks/<id>)에 없다
		stagedNotes      bool // 임시 자리에 작업 파일이 있다
		uidStillAssigned bool
	}
	var clears []clearCall
	var clearErr error
	uidAssigned := func(uid int) bool {
		var n int
		if err := m.pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE uid=$1`, uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	var current struct {
		id  string
		uid int
	}
	m.clearAgentDirs = func(_ context.Context, path string) error {
		rel, relErr := filepath.Rel(filepath.Join(m.dir, ".delete-staging"), path)
		_, workErr := os.Lstat(filepath.Join(m.dir, "tasks", current.id))
		notes, _ := filepath.Glob(filepath.Join(path, "*", "work", "notes.md"))
		clears = append(clears, clearCall{
			inDeleteStaging:  relErr == nil && !strings.HasPrefix(rel, ".."),
			workspaceMoved:   os.IsNotExist(workErr),
			stagedNotes:      len(notes) == 1,
			uidStillAssigned: uidAssigned(current.uid),
		})
		return clearErr
	}

	newTask := func(t *testing.T, name string) (string, int64, int) {
		t.Helper()
		task, taskID := newAgentUIDTestTask(t, m, name)
		uid, err := m.pg.AcquireTaskAgentUID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		work := filepath.Join(m.dir, "tasks", task.ID, "work")
		if err := os.MkdirAll(work, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("task notes"), 0o600); err != nil {
			t.Fatal(err)
		}
		current.id, current.uid = task.ID, uid
		return task.ID, taskID, uid
	}
	deleteWithFiles := func(t *testing.T, id string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/tasks/"+id, strings.NewReader(`{"delete_files":true}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	wantCall := []clearCall{{inDeleteStaging: true, workspaceMoved: true, stagedNotes: true, uidStillAssigned: true}}

	t.Run("clears the agent's directories while the uid is still assigned, then returns it", func(t *testing.T) {
		clears, clearErr = nil, nil
		id, taskID, uid := newTask(t, "clear then return")
		rec := deleteWithFiles(t, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("delete status = %d %s", rec.Code, rec.Body.String())
		}
		if !slices.Equal(clears, wantCall) {
			t.Fatalf("agent directory clears = %+v, want %+v", clears, wantCall)
		}
		if task, err := m.pg.GetTask(taskID); err != nil || task != nil {
			t.Fatalf("task was not deleted: %+v, %v", task, err)
		}
		if uidAssigned(uid) {
			t.Fatalf("uid %d still assigned after the task was deleted", uid)
		}
		if _, err := os.Lstat(filepath.Join(m.dir, "tasks", id)); !os.IsNotExist(err) {
			t.Fatalf("task files were not deleted: %v", err)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(m.dir, ".delete-staging", "task-"+id+"-*")); len(leftovers) != 0 {
			t.Fatalf("delete staging left %q", leftovers)
		}
	})

	t.Run("keeps the task, its uid and its files when the directories cannot be cleared", func(t *testing.T) {
		clears, clearErr = nil, errors.New("helper failed")
		t.Cleanup(func() { clearErr = nil })
		id, taskID, uid := newTask(t, "clear refused")
		rec := deleteWithFiles(t, id)
		if rec.Code != http.StatusConflict {
			t.Fatalf("delete status = %d %s, want 409", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "에이전트가 권한을 바꾼 파일을 지우지 못해 작업을 지우지 않았습니다") {
			t.Fatalf("delete body = %s, want the agent files message", body)
		}
		if !slices.Equal(clears, wantCall) {
			t.Fatalf("agent directory clears = %+v, want %+v", clears, wantCall)
		}
		if task, err := m.pg.GetTask(taskID); err != nil || task == nil {
			t.Fatalf("task was deleted: %+v, %v", task, err)
		}
		if !uidAssigned(uid) {
			t.Fatalf("uid %d was returned although the delete was refused", uid)
		}
		if got, err := os.ReadFile(filepath.Join(m.dir, "tasks", id, "work", "notes.md")); err != nil || string(got) != "task notes" {
			t.Fatalf("task files were not restored: %q, %v", got, err)
		}
	})
}

// TestRecoverTaskDeleteStages 는 시작할 때 작업 삭제의 임시 자리에 남은 것 가운데, 작업 행이 DB에서 지워진 것만
// 지우는지 본다. 작업 행이 남아 있으면(DB 삭제 전에 멈춤. 그 뒤 보관해 deleted_at만 찍힌 작업 포함) 그 파일은 아직
// 지우지 않은 작업의 것이라 둔다. 임시 자리나 그 안의 링크는 링크만 지우고 대상은 따라가지 않는다.
func TestRecoverTaskDeleteStages(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", config.ReadSecretEnv())
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	live, _ := newAgentUIDTestTask(t, m, "still registered")
	archived, archivedID := newAgentUIDTestTask(t, m, "archived")
	// 보관은 행을 남기고 deleted_at만 찍는다(db.task_archives_restore).
	if _, err := m.PG().Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, archivedID); err != nil {
		t.Fatal(err)
	}
	gone, goneID := newAgentUIDTestTask(t, m, "deleted")
	if err := m.PG().DeleteTask(goneID); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(m.dir, taskDeleteStagingDir)
	stages := map[string]bool{ // 이름 → 남아야 하는가
		"task-" + live.ID + "-111":     true,
		"task-" + archived.ID + "-444": true,
		"task-" + gone.ID + "-222":     false,
		"task-007-333":                 true, // 작업 id 모양이 아니다
		"other":                        true,
	}
	for name := range stages {
		if err := os.MkdirAll(filepath.Join(parent, name, "0-"+name, "work"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// 링크: 지운 작업의 임시 자리 안에서 밖(살아 있는 작업의 파일, 데이터 디렉터리 밖)을 가리키는 링크와, 임시 자리 자체가
	// 링크인 것. 지울 때 링크만 지우고 대상은 그대로여야 한다.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "keep")
	if err := os.WriteFile(outsideFile, []byte("KEEP-OUT"), 0o600); err != nil {
		t.Fatal(err)
	}
	liveWork := filepath.Join(m.dir, "tasks", live.ID, "work")
	if err := os.MkdirAll(liveWork, 0o700); err != nil {
		t.Fatal(err)
	}
	liveFile := filepath.Join(liveWork, "keep")
	if err := os.WriteFile(liveFile, []byte("KEEP-LIVE"), 0o600); err != nil {
		t.Fatal(err)
	}
	goneWork := filepath.Join(parent, "task-"+gone.ID+"-222", "0-task-"+gone.ID+"-222", "work")
	for name, target := range map[string]string{"to-live": liveWork, "to-outside": outside, "to-file": outsideFile} {
		if err := os.Symlink(target, filepath.Join(goneWork, name)); err != nil {
			t.Fatal(err)
		}
	}
	linkStages := map[string]string{ // 임시 자리 이름 → 가리키는 곳
		"task-" + gone.ID + "-555": liveWork,
		"task-" + gone.ID + "-666": outside,
	}
	for name, target := range linkStages {
		if err := os.Symlink(target, filepath.Join(parent, name)); err != nil {
			t.Fatal(err)
		}
		stages[name] = false
	}

	if err := recoverTaskDeleteStages(context.Background(), m.dir, m.PG()); err != nil {
		t.Fatal(err)
	}
	for name, wantKept := range stages {
		_, err := os.Lstat(filepath.Join(parent, name))
		if kept := err == nil; kept != wantKept {
			t.Errorf("%s kept = %v (%v), want %v", name, kept, err, wantKept)
		}
	}
	for path, want := range map[string]string{liveFile: "KEEP-LIVE", outsideFile: "KEEP-OUT"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("link target %s = %q, %v; want %q", path, got, err, want)
		}
	}
}
