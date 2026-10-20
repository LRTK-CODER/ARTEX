package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

// TestDeleteConversationClosesWorkspaceBeforeReturningUID 는 uid 분리를 켰을 때 대화를 지우면 그 작업 공간
// (sessions/conv-<id>, sessions/cmd-output/conv-<id>)을 서버 전용으로 닫은 뒤에 uid를 풀로 돌려주는지 본다(#182).
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
		done := make(chan *httptest.ResponseRecorder)
		go func() { done <- deleteConversation(t, c.ID) }()
		select {
		case rec := <-done:
			t.Fatalf("delete finished while the turn was running: %d %s", rec.Code, rec.Body.String())
		default:
		}
		if acl := readACLOf(t, dirs[0], agent.ACLAccess); len(acl) == 0 {
			t.Fatal("workspace was closed while the turn was running")
		}
		s.chatMu.Lock()
		delete(s.chatBusy, key)
		s.chatMu.Unlock()
		rec := <-done
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
}
