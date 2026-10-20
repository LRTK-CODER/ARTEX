package db

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"sync"
	"testing"
)

// 도우미(cmd/artex-sandbox/policy.go minAgentID·maxAgentID)의 허용 범위. 패키지 main이라 import할 수 없어 옮겨 적는다.
const (
	sandboxHelperMinID = 20000
	sandboxHelperMaxID = 29999
)

// readRepoFile 은 저장소 루트의 파일을 읽는다. 테스트는 패키지 디렉터리(db)에서 돈다.
func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func matchInt(t *testing.T, text, pattern string) int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("pattern %q not found", pattern)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// 풀 범위가 도우미 허용 범위 안이고, #175 공용 에이전트 사용자(이미지 기본 20000)와 겹치지 않고, 스키마 CHECK와
// 풀 사용자 스크립트가 같은 범위를 쓰는지 본다. 공용 사용자와 겹치면 2단계(#177)에서 그 uid를 받은 작업이 공용
// 그룹(setgid 작업 공간)으로 다른 작업 공간에 닿는다.
func TestAgentUIDPoolRange(t *testing.T) {
	if AgentUIDPoolSize != 1024 {
		t.Fatalf("pool size = %d, want 1024", AgentUIDPoolSize)
	}
	if AgentUIDPoolFirst < sandboxHelperMinID || AgentUIDPoolLast > sandboxHelperMaxID {
		t.Fatalf("pool %d-%d is outside the sandbox helper range %d-%d", AgentUIDPoolFirst, AgentUIDPoolLast, sandboxHelperMinID, sandboxHelperMaxID)
	}
	sharedAgentID := matchInt(t, readRepoFile(t, "Dockerfile"), `(?m)^ARG ARTEX_AGENT_UID=(\d+)$`)
	if sharedAgentID >= AgentUIDPoolFirst && sharedAgentID <= AgentUIDPoolLast {
		t.Fatalf("shared agent uid/gid %d (Dockerfile ARTEX_AGENT_UID) is inside the pool %d-%d", sharedAgentID, AgentUIDPoolFirst, AgentUIDPoolLast)
	}
	m := regexp.MustCompile(`CHECK \(uid BETWEEN (\d+) AND (\d+)\)`).FindStringSubmatch(schemaSQL)
	if m == nil {
		t.Fatal("agent_uids CHECK not found in schema.sql")
	}
	if m[1] != strconv.Itoa(AgentUIDPoolFirst) || m[2] != strconv.Itoa(AgentUIDPoolLast) {
		t.Fatalf("schema CHECK %s-%s, want %d-%d", m[1], m[2], AgentUIDPoolFirst, AgentUIDPoolLast)
	}
	script := readRepoFile(t, "agent-pool-users.sh")
	if first := matchInt(t, script, `(?m)^AGENT_POOL_FIRST=(\d+)$`); first != AgentUIDPoolFirst {
		t.Fatalf("agent-pool-users.sh first = %d, want %d", first, AgentUIDPoolFirst)
	}
	if size := matchInt(t, script, `(?m)^AGENT_POOL_SIZE=(\d+)$`); size != AgentUIDPoolSize {
		t.Fatalf("agent-pool-users.sh size = %d, want %d", size, AgentUIDPoolSize)
	}
}

func TestAgentUIDLockKeyNamespace(t *testing.T) {
	for _, reserved := range []int64{schemaMigrationLockKey, 7337741002, companyScopeMutationLock, findingEvidenceLockKey, 7337741010} {
		if agentUIDLockKey == reserved {
			t.Fatalf("agent uid lock key %d collides with another advisory lock", agentUIDLockKey)
		}
	}
}

func newAgentUIDTestConversation(t *testing.T, d *DB) int64 {
	t.Helper()
	c, err := d.CreateConversation("agent-uid-test", "agent uid test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(c.ID) })
	return c.ID
}

func agentUIDExists(t *testing.T, d *DB, uid int) bool {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM agent_uids WHERE uid=$1`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestAcquireAgentUIDDistinctStableAndReturnedOnDelete(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	task, err := d.CreateTaskWithOptions("agent uid", "agent uid", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	convID := newAgentUIDTestConversation(t, d)

	taskUID, err := d.AcquireTaskAgentUID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	convUID, err := d.AcquireConversationAgentUID(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if taskUID == convUID {
		t.Fatalf("task and conversation share uid %d", taskUID)
	}
	for _, uid := range []int{taskUID, convUID} {
		if uid < AgentUIDPoolFirst || uid > AgentUIDPoolLast {
			t.Fatalf("uid %d outside pool", uid)
		}
	}
	// 다시 받으면(재시작 뒤 다시 부르는 경우와 같다) 같은 uid다.
	if again, err := d.AcquireTaskAgentUID(ctx, task.ID); err != nil || again != taskUID {
		t.Fatalf("second acquire = %d, %v; want %d", again, err, taskUID)
	}
	if again, err := d.AcquireConversationAgentUID(ctx, convID); err != nil || again != convUID {
		t.Fatalf("second conversation acquire = %d, %v; want %d", again, err, convUID)
	}

	if err := d.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if agentUIDExists(t, d, taskUID) {
		t.Fatalf("uid %d still assigned after task delete", taskUID)
	}
	if err := d.DeleteConversation(convID); err != nil {
		t.Fatal(err)
	}
	if agentUIDExists(t, d, convUID) {
		t.Fatalf("uid %d still assigned after conversation delete", convUID)
	}
	// 지운 작업에는 새로 주지 않는다.
	if _, err := d.AcquireTaskAgentUID(ctx, task.ID); err == nil {
		t.Fatal("acquire for a deleted task succeeded")
	}
}

func TestAcquireAgentUIDRejectsOutOfPoolRows(t *testing.T) {
	d := openTestDB(t)
	convID := newAgentUIDTestConversation(t, d)
	for _, uid := range []int{AgentUIDPoolFirst - 1, AgentUIDPoolLast + 1} {
		if _, err := d.Exec(`INSERT INTO agent_uids(uid, conversation_id) VALUES($1,$2)`, uid, convID); err == nil {
			t.Fatalf("schema accepted uid %d outside the pool", uid)
		}
	}
	if _, err := d.Exec(`INSERT INTO agent_uids(uid) VALUES($1)`, AgentUIDPoolLast); err == nil {
		t.Fatal("schema accepted a uid row without an owner")
	}
}

func TestAcquireAgentUIDConcurrent(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	const owners = 24
	ids := make([]int64, owners)
	for i := range ids {
		ids[i] = newAgentUIDTestConversation(t, d)
	}
	// 소유자마다 두 번씩 동시에 받는다. 같은 소유자는 같은 uid, 다른 소유자는 다른 uid여야 한다.
	got := make([][2]int, owners)
	errs := make(chan error, owners*2)
	var wg sync.WaitGroup
	for i := range ids {
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				uid, err := d.AcquireConversationAgentUID(ctx, ids[i])
				if err != nil {
					errs <- err
					return
				}
				got[i][j] = uid
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[int]int64{}
	for i, pair := range got {
		if pair[0] != pair[1] {
			t.Fatalf("conversation %d got two uids %v", ids[i], pair)
		}
		if other, ok := seen[pair[0]]; ok {
			t.Fatalf("uid %d given to conversations %d and %d", pair[0], other, ids[i])
		}
		seen[pair[0]] = ids[i]
	}
}

func TestAcquireAgentUIDExhaustedFailsWithoutWaiting(t *testing.T) {
	if testing.Short() {
		t.Skip("풀 전체(1024개)를 채우는 느린 테스트")
	}
	d := openTestDB(t)
	ctx := t.Context()
	var used int
	if err := d.QueryRow(`SELECT count(*) FROM agent_uids`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	var last int64
	for i := used; i < AgentUIDPoolSize; i++ {
		last = newAgentUIDTestConversation(t, d)
		if _, err := d.AcquireConversationAgentUID(ctx, last); err != nil {
			t.Fatalf("fill %d/%d: %v", i+1, AgentUIDPoolSize, err)
		}
	}
	extra := newAgentUIDTestConversation(t, d)
	if _, err := d.AcquireConversationAgentUID(ctx, extra); !errors.Is(err, ErrAgentUIDPoolExhausted) {
		t.Fatalf("acquire on a full pool = %v, want ErrAgentUIDPoolExhausted", err)
	}
	// 이미 받은 소유자는 풀이 차 있어도 자기 uid를 그대로 받는다.
	if _, err := d.AcquireConversationAgentUID(ctx, last); err != nil {
		t.Fatalf("existing owner on a full pool: %v", err)
	}
	// 하나를 지우면 그 uid가 돌아와 다음 할당이 된다.
	freed, err := d.AcquireConversationAgentUID(ctx, last)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteConversation(last); err != nil {
		t.Fatal(err)
	}
	if uid, err := d.AcquireConversationAgentUID(ctx, extra); err != nil || uid != freed {
		t.Fatalf("acquire after delete = %d, %v; want freed uid %d", uid, err, freed)
	}
}

func TestAgentUIDKeptThroughArchiveRestoreAndReturnedOnArchiveDelete(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTaskWithOptions("agent uid archive", "agent uid archive", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_, _ = d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
	})
	uid, err := d.AcquireTaskAgentUID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := d.ClaimTaskArchiveJob(ctx); err != nil || claimed == nil || claimed.ID != archive.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/agent-uid-test.tar.zst", "abc", 1, 1); err != nil {
		t.Fatal(err)
	}
	assertTaskUID := func(stage string) {
		t.Helper()
		var got int
		if err := d.QueryRow(`SELECT uid FROM agent_uids WHERE task_id=$1`, task.ID).Scan(&got); err != nil || got != uid {
			t.Fatalf("%s: uid = %d, %v; want %d", stage, got, err, uid)
		}
	}
	assertTaskUID("archived")

	if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	if claimed, err := d.ClaimTaskArchiveJob(ctx); err != nil || claimed == nil || claimed.State != Restoring {
		t.Fatalf("restore claim = %+v, %v", claimed, err)
	}
	if _, err := d.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	assertTaskUID("restored")
	if again, err := d.AcquireTaskAgentUID(ctx, task.ID); err != nil || again != uid {
		t.Fatalf("acquire after restore = %d, %v; want %d", again, err, uid)
	}

	// 보관한 채로 지우는 경로(보관 꾸러미 삭제)에서도 uid가 돌아온다.
	archive, err = d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err = d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/agent-uid-test.tar.zst", "abc", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueTaskArchiveDelete(archive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteTaskArchiveStub(archive.ID); err != nil {
		t.Fatal(err)
	}
	if agentUIDExists(t, d, uid) {
		t.Fatalf("uid %d still assigned after archive delete", uid)
	}
}

// ListAgentUIDs 는 할당한 uid를 소유자 종류와 함께 돌려주고, 지운 작업의 uid는 빠진다(시작 때 작업 공간 ACL 재적용,
// #182). 다른 테스트가 같은 DB에 할당을 남길 수 있어 이 테스트가 만든 소유자만 본다.
func TestListAgentUIDs(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	task, err := d.CreateTaskWithOptions("agent uid list", "agent uid list", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	convID := newAgentUIDTestConversation(t, d)
	taskUID, err := d.AcquireTaskAgentUID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	convUID, err := d.AcquireConversationAgentUID(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	find := func() (got map[int]AgentUIDAssignment) {
		t.Helper()
		all, err := d.ListAgentUIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = map[int]AgentUIDAssignment{}
		for _, a := range all {
			if a.TaskID == task.ID || a.ConversationID == convID {
				got[a.UID] = a
			}
		}
		return got
	}
	want := map[int]AgentUIDAssignment{
		taskUID: {UID: taskUID, TaskID: task.ID},
		convUID: {UID: convUID, ConversationID: convID},
	}
	if got := find(); len(got) != 2 || got[taskUID] != want[taskUID] || got[convUID] != want[convUID] {
		t.Fatalf("ListAgentUIDs = %+v, want %+v", got, want)
	}
	if err := d.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if got := find(); len(got) != 1 || got[convUID] != want[convUID] {
		t.Fatalf("after task delete ListAgentUIDs = %+v, want only the conversation", got)
	}
}
