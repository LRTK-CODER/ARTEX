package server

import (
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// 에이전트 uid 분리(#165)를 켠 서버 umask(002)에서 작업 공간에 올린 파일은 0664다. 서버가 임시 파일에 쓴 뒤 이름을
// 바꿔 넣어도(#176), 같은 작업 디렉터리의 다른 파일처럼 에이전트 그룹이 그 자리에서 고칠 수 있어야 한다.
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
