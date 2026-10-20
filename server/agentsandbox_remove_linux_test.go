package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
)

// agentMarkMode 는 단위 테스트에서 "에이전트 소유"를 표시하는 디렉터리 모드다. 테스트는 root가 아니라 다른 uid 소유
// 디렉터리를 만들 수 없어, 소유자 대신 모드로 가른다. 서버 소유 디렉터리는 0755로 만든다.
const agentMarkMode = 0o711

func isAgentMarked(st *syscall.Stat_t) bool { return st.Mode&0o777 == agentMarkMode }

// agentDirVisit 은 walkAgentOwnedDirs가 visit에 넘긴 값이다. dir은 root 기준 경로다.
type agentDirVisit struct {
	dir   string
	names []string
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
// 않고 바로 위 디렉터리마다 묶어 넘기며, 링크는 따라가지 않는지 본다(#179).
func TestWalkAgentOwnedDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stage")
	mkdirMode(t, filepath.Join(root, "a", "agent1", "inner"), 0o755)
	mkdirMode(t, filepath.Join(root, "a", "agent1"), agentMarkMode)
	mkdirMode(t, filepath.Join(root, "a", "agent2"), agentMarkMode)
	mkdirMode(t, filepath.Join(root, "b", "deep", "agent3"), agentMarkMode)
	mkdirMode(t, filepath.Join(root, "top"), agentMarkMode)
	outside := filepath.Join(t.TempDir(), "outside")
	mkdirMode(t, filepath.Join(outside, "victim"), agentMarkMode)
	if err := os.WriteFile(filepath.Join(root, "a", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	var visits []agentDirVisit
	err := walkAgentOwnedDirs(root, isAgentMarked, func(dir *os.File, uid int, names []string) {
		if uid != os.Getuid() {
			t.Errorf("uid = %d, want the owner from fstat (%d)", uid, os.Getuid())
		}
		rel, _ := filepath.Rel(root, dir.Name())
		visits = append(visits, agentDirVisit{dir: rel, names: names})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []agentDirVisit{{dir: "a", names: []string{"agent1", "agent2"}}, {dir: filepath.Join("b", "deep"), names: []string{"agent3"}}, {dir: ".", names: []string{"top"}}}
	if !slices.EqualFunc(visits, want, func(a, b agentDirVisit) bool { return a.dir == b.dir && slices.Equal(a.names, b.names) }) {
		t.Fatalf("visits = %+v, want %+v", visits, want)
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
	err := walkAgentOwnedDirs(root, isAgentMarked, func(dir *os.File, _ int, names []string) {
		for _, name := range names {
			visited = append(visited, filepath.Join(dir.Name(), name))
			// 넘긴 디렉터리 fd 기준으로 지운다(도우미가 fd 3에서 하는 것과 같다).
			if err := os.RemoveAll(filepath.Join("/proc/self/fd", strconv.Itoa(int(dir.Fd())), name)); err != nil {
				t.Errorf("remove %s: %v", name, err)
			}
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

// TestWalkAgentOwnedDirsRejectsLinkRoot 는 지울 자리 자체가 링크면 따라가지 않고 오류인지 본다.
func TestWalkAgentOwnedDirsRejectsLinkRoot(t *testing.T) {
	base := t.TempDir()
	mkdirMode(t, filepath.Join(base, "outside", "victim"), agentMarkMode)
	root := filepath.Join(base, "stage")
	if err := os.Symlink(filepath.Join(base, "outside"), root); err != nil {
		t.Fatal(err)
	}
	called := false
	err := walkAgentOwnedDirs(root, isAgentMarked, func(*os.File, int, []string) { called = true })
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
