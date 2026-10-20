package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestPrepareSharedWorkspace 는 작업 공간이 에이전트 그룹의 setgid 디렉터리가 되고, 기능을 켜기 전에 서버가
// 만든 파일에 그룹 쓰기가 붙는지 본다. 작업 공간 밖을 가리키는 링크를 따라가 밖의 권한을 바꾸지 않는지,
// 데이터 디렉터리의 나머지 파일은 그대로인지도 본다. 테스트 사용자는 다른 그룹으로 chown할 수 없을 수 있어
// 자기 주 그룹을 에이전트 그룹으로 쓴다(그룹 변경 자체는 컨테이너 확인에서 본다).
func TestPrepareSharedWorkspace(t *testing.T) {
	gid := os.Getgid()
	dataDir := filepath.Join(t.TempDir(), "data")
	outside := filepath.Join(t.TempDir(), "oauth.key")
	mkdirWithMode(t, filepath.Join(dataDir, "tasks", "7", "i3"), 0o755)
	writeFileWithMode(t, filepath.Join(dataDir, "tasks", "7", "i3", "notes.md"), 0o644)
	writeFileWithMode(t, filepath.Join(dataDir, "traffic.db"), 0o644)
	writeFileWithMode(t, outside, 0o600)
	if err := os.Symlink(outside, filepath.Join(dataDir, "tasks", "7", "key-link")); err != nil {
		t.Fatal(err)
	}
	// 그룹·다른 사용자 쓰기가 있는 데이터 디렉터리도 통과만 남아야 한다. 에이전트 그룹이 나머지 항목을 바꿔치기하지 못하게.
	if err := os.Chmod(dataDir, 0o777); err != nil {
		t.Fatal(err)
	}

	if err := prepareSharedWorkspace(dataDir, gid); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want fs.FileMode
	}{
		{path: dataDir, want: 0o715},
		{path: filepath.Join(dataDir, "tasks"), want: fs.ModeSetgid | 0o775},
		{path: filepath.Join(dataDir, "sessions"), want: fs.ModeSetgid | 0o775},
		{path: filepath.Join(dataDir, "drafts"), want: fs.ModeSetgid | 0o775},
		{path: filepath.Join(dataDir, "tasks", "7", "i3"), want: fs.ModeSetgid | 0o775},
		{path: filepath.Join(dataDir, "tasks", "7", "i3", "notes.md"), want: 0o664},
		{path: filepath.Join(dataDir, "traffic.db"), want: 0o644},
		{path: outside, want: 0o600},
	}
	for _, tc := range cases {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid); got != tc.want {
			t.Errorf("%s mode = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func mkdirWithMode(t *testing.T, path string, perm fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func writeFileWithMode(t *testing.T, path string, perm fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}
