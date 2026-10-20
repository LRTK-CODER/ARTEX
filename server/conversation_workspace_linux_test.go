package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

// TestDeleteConversationClosesWorkspaceBeforeReturningUID 는 uid 분리를 켰을 때 대화를 지우면 그 작업 공간
// (sessions/conv-<id>, sessions/cmd-output/conv-<id>, 명령의 HOME·TMPDIR sessions/home·tmp/conv-<id>)을 서버 전용으로
// 닫은 뒤에 uid를 풀로 돌려주는지 본다(#182, #178).
// 닫지 않으면 같은 uid를 다시 받은 작업·세션이 지운 대화의 작업 공간을 읽는다. 닫지 못하면 대화를 지우지 않아 uid도
// 돌아가지 않는다. 돌고 있는 차례가 있으면 끝난 뒤에 닫는다.
func TestDeleteConversationClosesWorkspaceBeforeReturningUID(t *testing.T) {
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
	sessions := filepath.Join(m.dir, "sessions")
	// 실제 StopProcesses는 그 uid가 되어 kill(-1)을 보내므로 여기서는 돌리지 않는다. 불린 시점에 작업 공간이 이미 닫혔고
	// uid가 아직 할당돼 있었는지 남긴다. stopErr가 있으면 끝내지 못한 것으로 돌려준다(#179).
	type stopCall struct {
		uid              int
		workspaceClosed  bool
		uidStillAssigned bool
	}
	var stops []stopCall
	var stopErr error
	stopDirs := map[int]string{}
	s.stopAgentUID = func(_ context.Context, uid int) error {
		var n int
		if err := m.pg.QueryRow(`SELECT count(*) FROM agent_uids WHERE uid=$1`, uid).Scan(&n); err != nil {
			return err
		}
		closed := true
		if dir, ok := stopDirs[uid]; ok {
			// 삭제 요청을 처리하는 goroutine에서 불리므로 t.Fatal 대신 오류를 돌려준다. 그러면 삭제가 409로 드러난다.
			f, err := os.Open(dir)
			if err != nil {
				return err
			}
			_, aclErr := agent.GetACL(f, agent.ACLAccess)
			f.Close()
			if aclErr != nil && !errors.Is(aclErr, agent.ErrNoACL) {
				return aclErr
			}
			closed = errors.Is(aclErr, agent.ErrNoACL)
		}
		stops = append(stops, stopCall{uid: uid, workspaceClosed: closed, uidStillAssigned: n > 0})
		return stopErr
	}

	// newConversation 은 uid를 받은 대화와, 그 uid에 열린 작업 공간·큰 출력 디렉터리를 만든다.
	newConversation := func(t *testing.T, title string) (*db.Conversation, int, []string) {
		t.Helper()
		c, err := m.pg.CreateConversation("chat", title, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.pg.DeleteConversation(c.ID) })
		uid, err := m.pg.AcquireConversationAgentUID(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		key := s.convBusyKey(c.ID)
		dirs := []string{filepath.Join(sessions, key), filepath.Join(sessions, agent.ToolOutputDirName, key)}
		if err := agent.EnsureContainerDir(filepath.Dir(dirs[1])); err != nil {
			t.Fatal(err)
		}
		for i, dir := range dirs {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			access := agent.WorkspaceWrite
			if i == 1 {
				access = agent.WorkspaceRead
			}
			if err := agent.ShareDir(dir, uid, access); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := agent.EnsureCommandDirs(m.dir, agent.RunInfo{SessionID: key}, uid); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, filepath.Join(sessions, agent.CommandHomeDirName, key), filepath.Join(sessions, agent.CommandTmpDirName, key))
		stopDirs[uid] = dirs[0]
		return c, uid, dirs
	}
	deleteConversation := func(t *testing.T, id int64) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/conversations/"+strconv.FormatInt(id, 10), nil)
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

	t.Run("closes the workspace, then returns the uid", func(t *testing.T) {
		c, uid, dirs := newConversation(t, "close then return")
		// 돌고 있는 차례가 있는 것처럼 바쁨 표시를 둔다. 삭제는 표시가 풀릴 때까지 닫지도 지우지도 않는다.
		key := s.convBusyKey(c.ID)
		s.chatMu.Lock()
		s.chatBusy[key] = true
		s.chatMu.Unlock()
		// 확인 박자를 테스트가 보낸다. 박자를 받아 간 것은 삭제가 바쁨 표시를 확인하고 아직 기다린다는 뜻이다.
		ticks := make(chan time.Time)
		s.stopTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
		t.Cleanup(func() { s.stopTicker = newStopTicker })
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- deleteConversation(t, c.ID) }()
		for range 2 {
			select {
			case ticks <- time.Now():
			case rec := <-done:
				t.Fatalf("delete finished while the turn was running: %d %s", rec.Code, rec.Body.String())
			}
		}
		// 두 번째 박자까지 받아 갔으니 삭제는 바쁨 표시를 본 채 기다리는 중이고, 아직 닫지 않았다.
		if acl := readACLOf(t, dirs[0], agent.ACLAccess); len(acl) == 0 {
			t.Fatal("workspace was closed while the turn was running")
		}
		s.chatMu.Lock()
		delete(s.chatBusy, key)
		s.chatMu.Unlock()
		var rec *httptest.ResponseRecorder
		for rec == nil {
			select {
			case ticks <- time.Now():
			case rec = <-done:
			}
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("delete status = %d %s", rec.Code, rec.Body.String())
		}
		for _, dir := range dirs {
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Errorf("%s mode = %v, want 0700", dir, info.Mode().Perm())
			}
			if acl := readACLOf(t, dir, agent.ACLAccess); acl != nil {
				t.Errorf("%s still has an access ACL %v naming the returned uid", dir, acl)
			}
		}
		if uidAssigned(t, uid) {
			t.Fatalf("uid %d still assigned after the conversation was deleted", uid)
		}
		// 남은 프로세스는 작업 공간을 닫은 뒤, uid를 돌려주기 전에 끝냈다(#179).
		if want := []stopCall{{uid: uid, workspaceClosed: true, uidStillAssigned: true}}; !slices.Equal(stops, want) {
			t.Fatalf("agent process stops = %+v, want %+v", stops, want)
		}
		stops = nil
		s.chatMu.Lock()
		busy := s.chatBusy[key]
		s.chatMu.Unlock()
		if busy {
			t.Fatal("delete left the busy marker held")
		}
		// 돌려받은 uid를 새 대화가 다시 받아도 옛 작업 공간에는 그 uid 항목이 없다(위에서 확인).
		next, err := m.pg.CreateConversation("chat", "reuses the uid", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.pg.DeleteConversation(next.ID) })
		reused, err := m.pg.AcquireConversationAgentUID(ctx, next.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("deleted conversation's uid %d, next conversation got %d", uid, reused)
	})

	t.Run("a turn that does not stop by the deadline keeps the conversation", func(t *testing.T) {
		c, uid, dirs := newConversation(t, "turn does not stop")
		key := s.convBusyKey(c.ID)
		s.chatMu.Lock()
		s.chatBusy[key] = true
		s.chatMu.Unlock()
		t.Cleanup(func() {
			s.chatMu.Lock()
			delete(s.chatBusy, key)
			s.chatMu.Unlock()
		})
		// 마감이 이미 지났으면 박자를 기다리지 않고 바로 거부한다. 일괄 삭제가 마감 하나를 나눠 쓰는 것이 이 때문에 성립한다.
		s.stopTicker = func() (<-chan time.Time, func()) { return nil, func() {} }
		t.Cleanup(func() { s.stopTicker = newStopTicker })
		_, err := s.closeConversationWorkspace(c.ID, time.Now().Add(-time.Second))
		if !errors.Is(err, errTurnDidNotStop) {
			t.Fatalf("close error = %v, want errTurnDidNotStop", err)
		}
		if got, want := workspaceCloseMessage("대화 작업 공간", err), "실행 중인 차례가 멈추지 않았습니다. 잠시 뒤 다시 지우세요"; got != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
		if acl := readACLOf(t, dirs[0], agent.ACLAccess); len(acl) == 0 {
			t.Fatal("workspace was closed although the turn did not stop")
		}
		if !uidAssigned(t, uid) {
			t.Fatalf("uid %d was returned although the turn did not stop", uid)
		}
	})

	t.Run("keeps the conversation and its uid when the workspace cannot be closed", func(t *testing.T) {
		c, uid, dirs := newConversation(t, "cannot close")
		// 작업 공간 자리에 링크가 있으면 따라가지 않고 닫기를 거부한다.
		if err := os.Remove(dirs[0]); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), dirs[0]); err != nil {
			t.Fatal(err)
		}
		rec := deleteConversation(t, c.ID)
		if rec.Code != http.StatusConflict {
			t.Fatalf("delete status = %d %s, want 409", rec.Code, rec.Body.String())
		}
		if got, err := m.pg.GetConversation(c.ID); err != nil || got == nil {
			t.Fatalf("conversation was deleted: %+v, %v", got, err)
		}
		if !uidAssigned(t, uid) {
			t.Fatalf("uid %d was returned although the workspace stayed open", uid)
		}
	})
	t.Run("keeps the conversation and its uid when its agent processes cannot be stopped", func(t *testing.T) {
		c, uid, _ := newConversation(t, "processes remain")
		stopErr = errAgentProcessesRemain
		t.Cleanup(func() { stopErr, stops = nil, nil })
		rec := deleteConversation(t, c.ID)
		if rec.Code != http.StatusConflict {
			t.Fatalf("delete status = %d %s, want 409", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "에이전트 프로세스를 끝내지 못해 지우지 않았습니다") {
			t.Fatalf("delete body = %s, want the agent process message", body)
		}
		if got, err := m.pg.GetConversation(c.ID); err != nil || got == nil {
			t.Fatalf("conversation was deleted: %+v, %v", got, err)
		}
		if !uidAssigned(t, uid) {
			t.Fatalf("uid %d was returned although its processes were not stopped", uid)
		}
		s.chatMu.Lock()
		busy := s.chatBusy[s.convBusyKey(c.ID)]
		s.chatMu.Unlock()
		if busy {
			t.Fatal("a refused delete left the busy marker held")
		}
	})

	t.Run("batch delete stops each conversation's processes and keeps the ones it could not stop", func(t *testing.T) {
		stopped, _, _ := newConversation(t, "batch stopped")
		kept, keptUID, _ := newConversation(t, "batch kept")
		s.stopAgentUID = func(_ context.Context, uid int) error {
			stops = append(stops, stopCall{uid: uid})
			if uid == keptUID {
				return errAgentProcessesRemain
			}
			return nil
		}
		t.Cleanup(func() { stops = nil })
		body := fmt.Sprintf(`{"ids":[%d,%d]}`, stopped.ID, kept.ID)
		req := httptest.NewRequest(http.MethodPost, "/api/conversations/delete/batch", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("batch status = %d %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Items []conversationDeleteItem `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Items) != 2 || !resp.Items[0].OK || resp.Items[1].OK || !strings.Contains(resp.Items[1].Error, "에이전트 프로세스를 끝내지 못해") {
			t.Fatalf("batch items = %+v, want the first deleted and the second kept for its processes", resp.Items)
		}
		if len(stops) != 2 {
			t.Fatalf("agent process stops = %+v, want one per conversation", stops)
		}
		if got, err := m.pg.GetConversation(kept.ID); err != nil || got == nil {
			t.Fatalf("conversation whose processes remain was deleted: %+v, %v", got, err)
		}
		if !uidAssigned(t, keptUID) {
			t.Fatalf("uid %d was returned although its processes were not stopped", keptUID)
		}
	})
}
