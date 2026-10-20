package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEnsureCommandDirs 는 uid 분리를 켰을 때 작업·세션마다 HOME·TMPDIR를 작업 공간 옆에 만들고(#178), 서버 소유
// 0700에 그 uid만 ACL로 읽고 쓰게 하며, 명령 환경에 그 경로를 싣는지 본다. 기본 ACL에 서버 uid가 있어 에이전트가
// 만든 항목도 서버가 지운다. 작업 실행은 tasks/<tid>도 그 uid가 지나가게 연다.
func TestEnsureCommandDirs(t *testing.T) {
	requireACLTempDir(t)
	server := uint32(os.Getuid())
	rx, rwx := ACLRead|ACLExecute, aclPermAll
	writeAccess := ACL{{Tag: ACLUserOwner, Perm: rwx}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: rwx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rwx}, {Tag: ACLOther}}
	writeDefault := append(slices.Clone(writeAccess), ACLEntry{Tag: ACLNamedUser, ID: server, Perm: rwx})
	readAccess := ACL{{Tag: ACLUserOwner, Perm: rwx}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: rx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rx}, {Tag: ACLOther}}
	cases := []struct {
		name      string
		ri        RunInfo
		home, tmp string // 데이터 디렉터리 기준
		readDirs  []string
		passDirs  []string // 모든 uid가 지나가기만 하는 공유 디렉터리(0711)
	}{
		{name: "task", ri: RunInfo{TaskID: 7, IntentID: 3}, home: "tasks/7/home", tmp: "tasks/7/tmp", readDirs: []string{"tasks/7"}, passDirs: []string{"tasks"}},
		{name: "session", ri: RunInfo{SessionID: "conv-9"}, home: "sessions/home/conv-9", tmp: "sessions/tmp/conv-9", passDirs: []string{"sessions", "sessions/home", "sessions/tmp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			at := func(rel string) string { return filepath.Join(data, filepath.FromSlash(rel)) }
			env, err := EnsureCommandDirs(data, tc.ri, testAgentUID)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{AgentHomeEnv + "=" + at(tc.home), "TMPDIR=" + at(tc.tmp)}
			if !slices.Equal(env, want) {
				t.Fatalf("env = %v, want %v", env, want)
			}
			for _, dir := range []string{tc.home, tc.tmp} {
				assertMode(t, at(dir), 0o770)
				wantACL(t, at(dir), ACLAccess, writeAccess)
				wantACL(t, at(dir), ACLDefault, writeDefault)
			}
			for _, dir := range tc.readDirs {
				wantACL(t, at(dir), ACLAccess, readAccess)
			}
			for _, dir := range tc.passDirs {
				assertMode(t, at(dir), 0o711)
				wantACL(t, at(dir), ACLAccess, nil)
			}
			// 여러 번 불러도 같다(실행마다 부른다).
			if again, err := EnsureCommandDirs(data, tc.ri, testAgentUID); err != nil || !slices.Equal(again, want) {
				t.Fatalf("second call = %v, %v", again, err)
			}
		})
	}
}

// TestEnsureCommandDirsRefuses 는 HOME 자리에 링크가 있거나 실행이 작업·세션에 속하지 않으면 만들지 않고 오류를
// 올리는지 본다. 링크를 따라가면 서버 uid가 밖의 디렉터리에 그 uid의 ACL을 건다.
func TestEnsureCommandDirsRefuses(t *testing.T) {
	requireACLTempDir(t)
	data := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "tasks", "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "tasks", "7", CommandHomeDirName)); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCommandDirs(data, RunInfo{TaskID: 7}, testAgentUID); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("planted HOME link: err = %v, want a refusal", err)
	}
	wantACL(t, outside, ACLAccess, nil)
	for _, ri := range []RunInfo{{}, {SessionID: ".."}, {SessionID: "a/b"}} {
		if _, err := EnsureCommandDirs(data, ri, testAgentUID); err == nil {
			t.Fatalf("EnsureCommandDirs(%+v) = nil error, want a refusal", ri)
		}
	}
}
