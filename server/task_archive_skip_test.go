package server

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	pgdb "github.com/Autumn-27/artex/db"
)

// TestTaskArchivePackageSkipsUnreadableEntries 는 서버가 읽지 못하는 파일·디렉터리가 있어도 보관이 실패하지 않고,
// 그 항목을 건너뛴 목록으로 돌려주는지 본다(#179). uid 분리를 켜면 에이전트가 자기 파일을 chmod 600·000으로 바꿔
// 서버가 읽지 못하게 할 수 있다. 여기서는 서버 소유 파일의 소유자 권한을 빼서 같은 상태(읽기 EACCES)를 만든다.
// 에이전트 uid 소유 파일로 보는 확인은 컨테이너 e2e(TestAgentSandboxEndToEnd)에 있다.
func TestTaskArchivePackageSkipsUnreadableEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not remove read access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}
	dataDir := t.TempDir()
	payload := filepath.Join(dataDir, "payload")
	work := filepath.Join(payload, "files", "tasks", "42", "work")
	locked := filepath.Join(work, "locked")
	if err := os.MkdirAll(locked, archiveDirMode); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(work, "open.txt"):    "keep me",
		filepath.Join(work, "secret.txt"):  "agent secret",
		filepath.Join(locked, "inner.txt"): "inner",
	} {
		if err := os.WriteFile(path, []byte(content), archiveFileMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(work, "secret.txt"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, archiveDirMode) }) // t.TempDir 정리가 안을 지울 수 있게 되돌린다

	archivePath := filepath.Join(dataDir, "archive.tar.zst")
	snapshot := &pgdb.TaskArchiveSnapshot{FormatVersion: pgdb.TaskArchiveFormatVersion, TaskID: 42}
	pkg, err := writeTaskArchivePackage(archivePath, payload, snapshot)
	if err != nil {
		t.Fatalf("archive failed instead of skipping unreadable entries: %v", err)
	}
	want := []string{"files/tasks/42/work/locked", "files/tasks/42/work/secret.txt"}
	if got := slices.Sorted(slices.Values(pkg.skipped)); !slices.Equal(got, want) {
		t.Fatalf("skipped = %q, want %q", got, want)
	}
	extracted := filepath.Join(dataDir, "restore")
	if err := extractTaskArchivePackage(archivePath, pkg.checksum, extracted); err != nil {
		t.Fatal(err)
	}
	restoredWork := filepath.Join(extracted, "files", "tasks", "42", "work")
	if got, err := os.ReadFile(filepath.Join(restoredWork, "open.txt")); err != nil || string(got) != "keep me" {
		t.Fatalf("readable file not archived: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(restoredWork, "secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("unreadable file is in the package: %v", err)
	}
	// 나열하지 못한 디렉터리는 항목만 들어가고 안은 빠진다.
	entries, err := os.ReadDir(filepath.Join(restoredWork, "locked"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("locked dir in package = %v, %v; want an empty directory", entries, err)
	}
}

func TestSkippedArchiveWarning(t *testing.T) {
	many := make([]string, maxSkippedArchiveListed+10)
	for i := range many {
		many[i] = fmt.Sprintf("files/tasks/1/work/f%d", i)
	}
	cases := []struct {
		name     string
		skipped  []string
		want     []string
		wantNone []string
	}{
		{name: "none"},
		{name: "few", skipped: []string{"files/tasks/1/work/a", "files/tasks/1/work/b"},
			want: []string{"2개", "files/tasks/1/work/a, files/tasks/1/work/b"}, wantNone: []string{" 외 "}},
		{name: "capped", skipped: many,
			want:     []string{fmt.Sprintf("%d개", len(many)), "files/tasks/1/work/f49", " 외 10개"},
			wantNone: []string{"files/tasks/1/work/f50"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skippedArchiveWarning(tc.skipped)
			if len(tc.skipped) == 0 {
				if got != "" {
					t.Fatalf("warning = %q, want none", got)
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("warning %q lacks %q", got, w)
				}
			}
			for _, w := range tc.wantNone {
				if strings.Contains(got, w) {
					t.Errorf("warning %q has %q", got, w)
				}
			}
		})
	}
}
