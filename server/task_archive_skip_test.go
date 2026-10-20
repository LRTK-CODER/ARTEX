package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	pgdb "github.com/Autumn-27/artex/db"
)

// TestTaskArchivePackageSkipsUnreadableEntries 는 uid 분리를 켰을 때(skipUnreadable) 서버가 읽지 못하는 파일·디렉터리가
// 있어도 보관이 실패하지 않고 그 항목을 건너뛴 목록으로 돌려주는지, 꺼져 있으면 예전처럼 보관이 실패하는지 본다(#179).
// uid 분리를 켜면 에이전트가 자기 파일을 chmod 600·000으로 바꿔 서버가 읽지 못하게 할 수 있다. 여기서는 서버 소유 파일의
// 소유자 권한을 빼서 같은 상태(읽기 EACCES)를 만든다. 꺼져 있으면 모든 파일이 서버 소유라 읽지 못하는 것은 결함이고,
// 건너뛰면 원본이 임시 디렉터리 정리로 사라진다. 에이전트 uid 소유 파일로 보는 확인은 컨테이너 e2e(TestAgentSandboxEndToEnd)에 있다.
func TestTaskArchivePackageSkipsUnreadableEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not remove read access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}
	// lockedPayload 는 읽을 수 있는 파일 하나와, 서버가 읽지 못하는 파일(000)·디렉터리(000) 하나씩을 둔 payload다.
	lockedPayload := func(t *testing.T) (dataDir, payload string) {
		t.Helper()
		dataDir = t.TempDir()
		payload = filepath.Join(dataDir, "payload")
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
		return dataDir, payload
	}
	snapshot := &pgdb.TaskArchiveSnapshot{FormatVersion: pgdb.TaskArchiveFormatVersion, TaskID: 42}

	t.Run("uid separation on skips them", func(t *testing.T) {
		dataDir, payload := lockedPayload(t)
		archivePath := filepath.Join(dataDir, "archive.tar.zst")
		pkg, err := writeTaskArchivePackage(archivePath, payload, snapshot, true)
		if err != nil {
			t.Fatalf("archive failed instead of skipping unreadable entries: %v", err)
		}
		want := []string{"files/tasks/42/work/locked", "files/tasks/42/work/secret.txt"}
		if got := slices.Sorted(slices.Values(pkg.skipped.names)); pkg.skipped.count != len(want) || !slices.Equal(got, want) {
			t.Fatalf("skipped = %d %q, want %q", pkg.skipped.count, got, want)
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
	})

	t.Run("uid separation off fails the archive", func(t *testing.T) {
		dataDir, payload := lockedPayload(t)
		archivePath := filepath.Join(dataDir, "archive.tar.zst")
		_, err := writeTaskArchivePackage(archivePath, payload, snapshot, false)
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("archive error = %v, want a permission error", err)
		}
		if _, err := os.Lstat(archivePath); !os.IsNotExist(err) {
			t.Fatalf("a failed archive left a package: %v", err)
		}
	})
}

// skippedEntries 는 에이전트가 정한 이름을 개수와 길이 상한까지만 기억하고 전체 개수는 따로 센다.
func TestSkippedEntriesCapsNamesAndLength(t *testing.T) {
	var e skippedEntries
	long := strings.Repeat("a", maxSkippedNameBytes+100)
	e.add(long)
	for i := range maxSkippedArchiveListed + 9 {
		e.add(fmt.Sprintf("f%d", i))
	}
	if e.count != maxSkippedArchiveListed+10 {
		t.Fatalf("count = %d, want %d", e.count, maxSkippedArchiveListed+10)
	}
	if len(e.names) != maxSkippedArchiveListed {
		t.Fatalf("kept %d names, want %d", len(e.names), maxSkippedArchiveListed)
	}
	if want := strings.Repeat("a", maxSkippedNameBytes) + "…"; e.names[0] != want {
		t.Fatalf("long name kept as %d bytes, want it cut to %d bytes plus a marker", len(e.names[0]), maxSkippedNameBytes)
	}
}

func TestSkippedArchiveWarning(t *testing.T) {
	var many skippedEntries
	for i := range maxSkippedArchiveListed + 10 {
		many.add(fmt.Sprintf("files/tasks/1/work/f%d", i))
	}
	var few skippedEntries
	few.add("files/tasks/1/work/a")
	few.add("files/tasks/1/work/b")
	var control skippedEntries
	control.add("files/tasks/1/work/line\nforged log line")
	cases := []struct {
		name     string
		skipped  skippedEntries
		want     []string
		wantNone []string
	}{
		{name: "none"},
		{name: "few", skipped: few,
			want: []string{"2개", `"files/tasks/1/work/a", "files/tasks/1/work/b"`}, wantNone: []string{" 외 "}},
		{name: "capped", skipped: many,
			want:     []string{fmt.Sprintf("%d개", many.count), `"files/tasks/1/work/f49"`, " 외 10개"},
			wantNone: []string{"files/tasks/1/work/f50"}},
		// 에이전트가 이름에 줄바꿈을 넣어도 경고와 로그는 한 줄이다.
		{name: "control characters are quoted", skipped: control,
			want: []string{`"files/tasks/1/work/line\nforged log line"`}, wantNone: []string{"\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skippedArchiveWarning(tc.skipped)
			if tc.skipped.count == 0 {
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
