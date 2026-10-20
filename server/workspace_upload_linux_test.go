package server

import (
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// 작업 공간에 올린 파일은 0666에서 umask(여기서는 002)를 뺀 0664다. 서버가 임시 파일에 쓴 뒤 이름을 바꿔 넣어도
// (#176), 같은 작업 디렉터리의 다른 파일처럼 그룹 자리(uid 분리를 켜면 ACL mask, #182)에 쓰기가 남아야 한다.
func TestWorkspaceUploadKeepsGroupWritableUnderSharedUmask(t *testing.T) {
	previous := syscall.Umask(0o002)
	t.Cleanup(func() { syscall.Umask(previous) })

	cases := []struct {
		name  string
		plant func(t *testing.T, work string)
	}{
		{name: "새 파일"},
		{
			name: "에이전트가 만든 0664 파일 덮어쓰기",
			plant: func(t *testing.T, work string) {
				path := filepath.Join(work, "report.txt")
				mustWrite(t, path, "agent")
				if err := os.Chmod(path, 0o664); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnitFixture(t)
			work := filepath.Join(f.data, "tasks", "1", "work")
			if tc.plant != nil {
				tc.plant(t, work)
			}
			rec := serveWorkspace(f.server.wsUpload, uploadRequest(t, "tasks/1/work", "report.txt"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
			}
			info, err := os.Lstat(filepath.Join(work, "report.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o664 {
				t.Fatalf("mode = %v, want -rw-rw-r--", info.Mode())
			}
		})
	}
}

// 웹 작업 공간 쓰기와 채팅 첨부도 작업 공간 안에 서버가 만드는 파일이라 같은 규칙(0666, umask가 정함)을 따른다.
// 채팅 첨부는 에이전트 작업 디렉터리(sessions/<id>/uploads)에 저장된다.
func TestServerCreatedWorkspaceFilesFollowUmask(t *testing.T) {
	previous := syscall.Umask(0o002)
	t.Cleanup(func() { syscall.Umask(previous) })

	t.Run("작업 공간 쓰기", func(t *testing.T) {
		f := newUnitFixture(t)
		rec := serveWorkspace(f.server.wsWrite, workspaceJSON("/api/workspace/write", `{"path":"tasks/1/work/new.txt","content":"x"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
		}
		assertMode(t, filepath.Join(f.data, "tasks", "1", "work", "new.txt"), 0o664)
	})
	t.Run("채팅 첨부", func(t *testing.T) {
		data := t.TempDir()
		s := &Server{m: &Manager{dir: data}}
		rec := serveWorkspace(s.chatUpload, chatUploadRequest(t, "s1", "body"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
		}
		assertMode(t, filepath.Join(data, "sessions", "s1", "uploads", "report.txt"), 0o664)
	})
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode = %v, want %v", got, want)
	}
}
