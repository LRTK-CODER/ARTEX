package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
)

// agentMarkMode·otherMarkMode 는 단위 테스트에서 "에이전트 소유"와 "그 밖 소유(root 등)"를 표시하는 디렉터리 모드다.
// 테스트는 root가 아니라 다른 uid 소유 디렉터리를 만들 수 없어, 소유자 대신 모드로 가른다. 서버 소유 디렉터리는 0755로
// 만든다. 실제 uid로 가르는 판단은 TestAgentDirOwner가 본다.
const (
	agentMarkMode = 0o711
	otherMarkMode = 0o751
)

func isAgentMarked(st *syscall.Stat_t) dirOwner {
	switch st.Mode & 0o777 {
	case agentMarkMode:
		return dirOwnedByAgent
	case otherMarkMode:
		return dirOwnedByOther
	default:
		return dirOwnedByServer
	}
}

// TestAgentDirOwner 는 지우기 걷기가 도우미에 넘기는 디렉터리를 도우미 허용 범위(20000-29999) 안 uid 소유로 한정하는지
// 본다(#179). 범위 밖(root 등) 소유를 넘기면 도우미가 거부해 파일을 지우는 작업 삭제가 409로 막힌다. 그런 디렉터리는
// 서버 uid로 지우게 남긴다.
func TestAgentDirOwner(t *testing.T) {
	server := os.Getuid()
	cases := []struct {
		name string
		uid  int
		want dirOwner
	}{
		{"server", server, dirOwnedByServer},
		{"root", 0, dirOwnedByOther},
		{"below the helper range", helperMinUID - 1, dirOwnedByOther},
		{"shared agent uid", helperMinUID, dirOwnedByAgent},
		{"pool uid", 20001, dirOwnedByAgent},
		{"top of the helper range", helperMaxUID, dirOwnedByAgent},
		{"above the helper range", helperMaxUID + 1, dirOwnedByOther},
		{"nobody", 65534, dirOwnedByOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.uid == server && tc.want != dirOwnedByServer {
				t.Skipf("the test runs as uid %d", server)
			}
			if got := agentDirOwner(&syscall.Stat_t{Uid: uint32(tc.uid)}); got != tc.want {
				t.Fatalf("agentDirOwner(uid %d) = %d, want %d", tc.uid, got, tc.want)
			}
		})
	}
}

// visitedPaths 는 walkAgentOwnedDirs가 visit에 넘긴 디렉터리들의 root 기준 경로다.
func visitedPaths(root string, dirs []*os.File) []string {
	paths := make([]string, len(dirs))
	for i, dir := range dirs {
		paths[i], _ = filepath.Rel(root, dir.Name())
	}
	return paths
}

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestWalkAgentOwnedDirs 는 지우기 걷기가 서버 소유 디렉터리로만 내려가고, 에이전트 소유 디렉터리는 안으로 들어가지
// 않고 바로 위 디렉터리마다 묶어 넘기며, 그 밖 소유 디렉터리와 링크는 건너뛰는지 본다(#179).
func TestWalkAgentOwnedDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stage")
	mkdirMode(t, filepath.Join(root, "a", "agent1", "inner"), 0o755)
	mkdirMode(t, filepath.Join(root, "a", "agent1"), agentMarkMode)
	mkdirMode(t, filepath.Join(root, "a", "agent2"), agentMarkMode)
	mkdirMode(t, filepath.Join(root, "b", "deep", "agent3"), agentMarkMode)
	mkdirMode(t, filepath.Join(root, "top"), agentMarkMode)
	// 그 밖 소유 디렉터리는 넘기지도, 안으로 내려가지도 않는다(안의 에이전트 디렉터리도 넘기지 않는다).
	mkdirMode(t, filepath.Join(root, "rootdir", "agent4"), agentMarkMode)
	mkdirMode(t, filepath.Join(root, "rootdir"), otherMarkMode)
	outside := filepath.Join(t.TempDir(), "outside")
	mkdirMode(t, filepath.Join(outside, "victim"), agentMarkMode)
	if err := os.WriteFile(filepath.Join(root, "a", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	var visits [][]string
	err := walkAgentOwnedDirs(root, isAgentMarked, func(uid int, dirs []*os.File) {
		if uid != os.Getuid() {
			t.Errorf("uid = %d, want the owner from fstat (%d)", uid, os.Getuid())
		}
		visits = append(visits, visitedPaths(root, dirs))
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{filepath.Join("a", "agent1"), filepath.Join("a", "agent2")}, {filepath.Join("b", "deep", "agent3")}, {"top"}}
	if !slices.EqualFunc(visits, want, slices.Equal) {
		t.Fatalf("visits = %q, want %q", visits, want)
	}
}

// TestWalkAgentOwnedDirsBatches 는 한 디렉터리에 에이전트 디렉터리가 많아도 visit 한 번에 agentRemoveBatch개까지만
// 넘겨, 서버가 한꺼번에 여는 fd가 묶이는지 본다.
func TestWalkAgentOwnedDirsBatches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stage")
	total := agentRemoveBatch + 3
	for i := range total {
		mkdirMode(t, filepath.Join(root, fmt.Sprintf("d%03d", i)), agentMarkMode)
	}
	var sizes []int
	err := walkAgentOwnedDirs(root, isAgentMarked, func(_ int, dirs []*os.File) { sizes = append(sizes, len(dirs)) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{agentRemoveBatch, 3}; !slices.Equal(sizes, want) {
		t.Fatalf("batch sizes = %v, want %v", sizes, want)
	}
}

// clearThroughFD 는 도우미가 agentRemoveScript에서 하는 것처럼, 넘겨받은 디렉터리 fd를 /proc/self/fd로 따라가 그 안을
// 비운다(경로를 다시 풀지 않는다).
func clearThroughFD(t *testing.T, dir *os.File) {
	t.Helper()
	proc := filepath.Join("/proc/self/fd", strconv.Itoa(int(dir.Fd())))
	entries, err := os.ReadDir(proc)
	if err != nil {
		t.Errorf("read %s through its fd: %v", dir.Name(), err)
		return
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(proc, entry.Name())); err != nil {
			t.Errorf("clear %s: %v", dir.Name(), err)
		}
	}
}

// TestWalkAgentOwnedDirsDoesNotFollowSwappedLink 은 걷는 도중 아직 내려가지 않은 서버 소유 디렉터리가 밖을 가리키는
// 링크로 바뀌어도 밖으로 내려가지 않고 밖의 디렉터리를 넘기지 않는지 본다(#179). 경로로 다시 여는 걷기(filepath.WalkDir)는
// 목록을 본 뒤 그 이름을 다시 열 때 링크를 따라가 밖의 에이전트 디렉터리를 지울 대상으로 넘긴다. 넘긴 dir로 실제로 지워
// 밖이 그대로인지도 본다.
func TestWalkAgentOwnedDirsDoesNotFollowSwappedLink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stage")
	mkdirMode(t, filepath.Join(root, "a", "agent1"), agentMarkMode)
	mkdirMode(t, filepath.Join(root, "b", "x", "agent2"), agentMarkMode)
	outside := filepath.Join(base, "outside")
	mkdirMode(t, filepath.Join(outside, "victim"), agentMarkMode)
	if err := os.WriteFile(filepath.Join(outside, "victim", "keep"), []byte("other task"), 0o644); err != nil {
		t.Fatal(err)
	}

	swapped := false
	var visited []string
	err := walkAgentOwnedDirs(root, isAgentMarked, func(_ int, dirs []*os.File) {
		for _, dir := range dirs {
			visited = append(visited, dir.Name())
			clearThroughFD(t, dir)
		}
		if swapped {
			return
		}
		// 첫 묶음(a)을 넘긴 시점에 b와 b/x는 아직 열지 않았다. b/x를 밖으로 옮기고 그 자리에 밖을 가리키는 링크를 둔다.
		swapped = true
		if err := os.Rename(filepath.Join(root, "b", "x"), filepath.Join(base, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "b", "x")); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(root, "a", "agent1")}; !slices.Equal(visited, want) {
		t.Fatalf("visited = %q, want only %q", visited, want)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "victim", "keep")); err != nil || string(got) != "other task" {
		t.Fatalf("outside directory was touched: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "moved", "agent2")); err != nil {
		t.Fatalf("the directory moved out during the walk was touched: %v", err)
	}
}

// TestWalkAgentOwnedDirsHandsOverTheOpenedDir 는 visit에 넘긴 fd가 걷기가 연 그 디렉터리를 가리키는지 본다(#179). 넘긴
// 뒤 이름이 밖을 가리키는 링크로 바뀌어도 fd로 비우면 밖은 그대로다. 위 디렉터리가 닫혀 있어도(시작 때 #186이 보관
// 임시 자리를 서버 전용으로 닫은 경우) fd로는 들어간다.
func TestWalkAgentOwnedDirsHandsOverTheOpenedDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stage")
	parent := filepath.Join(root, "work")
	mkdirMode(t, filepath.Join(parent, "agent"), agentMarkMode)
	if err := os.WriteFile(filepath.Join(parent, "agent", "secret"), []byte("deleted task"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	mkdirMode(t, outside, 0o755)
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("other task"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := walkAgentOwnedDirs(root, isAgentMarked, func(_ int, dirs []*os.File) {
		if err := os.Rename(filepath.Join(parent, "agent"), filepath.Join(base, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(parent, "agent")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(parent, 0o755) }() // t.TempDir 정리를 위해 되돌린다. 실패하면 정리가 알린다
		for _, dir := range dirs {
			clearThroughFD(t, dir)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(got) != "other task" {
		t.Fatalf("the link target was cleared: %q, %v", got, err)
	}
	if entries, err := os.ReadDir(filepath.Join(base, "moved")); err != nil || len(entries) != 0 {
		t.Fatalf("the opened directory was not cleared: %v, %v", entries, err)
	}
}

// TestWalkAgentOwnedDirsRejectsLinkRoot 는 지울 자리 자체가 링크면 따라가지 않고 오류인지 본다.
func TestWalkAgentOwnedDirsRejectsLinkRoot(t *testing.T) {
	base := t.TempDir()
	mkdirMode(t, filepath.Join(base, "outside", "victim"), agentMarkMode)
	root := filepath.Join(base, "stage")
	if err := os.Symlink(filepath.Join(base, "outside"), root); err != nil {
		t.Fatal(err)
	}
	called := false
	err := walkAgentOwnedDirs(root, isAgentMarked, func(int, []*os.File) { called = true })
	if !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("walk of a link root = %v, want ELOOP or ENOTDIR", err)
	}
	if called {
		t.Fatal("walk followed the link root")
	}
}

// TestStopProcessesFailsWithoutHelper 는 도우미를 실행하지 못하면 끝냈다고 보지 않고 오류인지 본다. 호출자는 지우기를
// 멈춘다. 실제로 끝내는 확인은 컨테이너 e2e에 있다.
func TestStopProcessesFailsWithoutHelper(t *testing.T) {
	sb := &AgentSandbox{helper: filepath.Join(t.TempDir(), "missing-helper"), uid: 20000, gid: 20000}
	if err := sb.StopProcesses(context.Background(), 20001); err == nil {
		t.Fatal("StopProcesses without a helper succeeded")
	}
}
