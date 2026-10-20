package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AgentUIDPoolFirst·AgentUIDPoolSize 는 작업·대화형 세션에 나눠 주는 에이전트 uid 풀이다(#166).
// gid도 같은 값을 쓴다. 풀은 도우미(cmd/artex-sandbox/policy.go)의 허용 범위 20000-29999 안에 있어야 하고,
// #175의 공용 에이전트 사용자(Dockerfile ARTEX_AGENT_UID=20000)와 겹치지 않아야 한다. 겹치면 그 uid를 받은
// 작업이 공용 그룹으로 다른 작업 공간에 닿는다. 이미지·설치 스크립트(agent-pool-users.sh)가 이 범위의 passwd·group
// 항목을 만들고, schema.sql의 agent_uids CHECK도 같은 범위다. 바뀌지 않는 값이라 설정으로 빼지 않는다.
const (
	AgentUIDPoolFirst = 20001
	AgentUIDPoolSize  = 1024
)

// AgentUIDPoolLast 는 풀의 마지막 uid다.
const AgentUIDPoolLast = AgentUIDPoolFirst + AgentUIDPoolSize - 1

// ErrAgentUIDPoolExhausted 는 풀의 uid를 모두 나눠 줬을 때 올린다. 기다리지 않는다. 작업이나 대화를 지워야 uid가 돌아온다.
var ErrAgentUIDPoolExhausted = fmt.Errorf("agent uid pool %d-%d is exhausted; delete unused tasks or conversations to free a uid", AgentUIDPoolFirst, AgentUIDPoolLast)

// agentUIDLockKey 는 uid 할당을 줄 세우는 advisory lock 키다. 다른 키(스키마 …001, 테스트 …002, 회사 범위 …003,
// 증거 …004, 재검사 …010)와 겹치지 않는다.
const agentUIDLockKey int64 = 7337741005

// agentUIDOwnerColumn 은 agent_uids에서 소유자를 가리키는 열이다. 질의에 넣는 열 이름은 이 두 값뿐이다.
type agentUIDOwnerColumn string

const (
	agentUIDOwnerTask         agentUIDOwnerColumn = "task_id"
	agentUIDOwnerConversation agentUIDOwnerColumn = "conversation_id"
)

// AcquireTaskAgentUID 는 작업의 에이전트 uid를 돌려준다. 이미 받았으면 같은 값이고, 없으면 풀에서 가장 작은
// 빈 uid를 준다. 풀이 비면 ErrAgentUIDPoolExhausted, 작업이 없으면 외래 키 오류를 올린다.
func (d *DB) AcquireTaskAgentUID(ctx context.Context, taskID int64) (int, error) {
	return d.acquireAgentUID(ctx, agentUIDOwnerTask, taskID)
}

// AcquireConversationAgentUID 는 대화형 세션(conversations)의 에이전트 uid를 돌려준다. 동작은 AcquireTaskAgentUID와 같다.
func (d *DB) AcquireConversationAgentUID(ctx context.Context, conversationID int64) (int, error) {
	return d.acquireAgentUID(ctx, agentUIDOwnerConversation, conversationID)
}

func (d *DB) acquireAgentUID(ctx context.Context, owner agentUIDOwnerColumn, ownerID int64) (int, error) {
	// 이미 받은 작업·대화가 대부분이라 잠그지 않고 먼저 찾는다.
	var uid int
	err := d.QueryRowContext(ctx, `SELECT uid FROM agent_uids WHERE `+string(owner)+`=$1`, ownerID).Scan(&uid)
	if err == nil {
		return uid, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// 할당끼리 줄을 세워 두 할당이 같은 빈 uid를 고르지 못하게 한다. 표 잠금(LOCK TABLE) 대신 트랜잭션 advisory
	// lock을 쓰는 이유: 표 잠금은 작업 삭제의 cascade(ROW EXCLUSIVE)와 충돌한다. 삭제 트랜잭션은 tasks 행을 먼저
	// 잡고 있어, 그 작업에 uid를 넣으려는 할당(외래 키 검사)과 서로 기다리는 교착이 생긴다. 겹치지 않음은 uid
	// 기본 키와 소유자 UNIQUE 제약이 따로 보장한다. 할당은 작업·대화마다 한 번뿐이라 줄이 길지 않다.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, agentUIDLockKey); err != nil {
		return 0, fmt.Errorf("lock agent uid allocation: %w", err)
	}
	// 잠금을 기다리는 동안 같은 소유자의 다른 할당이 끝났을 수 있어 다시 찾는다.
	err = tx.QueryRowContext(ctx, `SELECT uid FROM agent_uids WHERE `+string(owner)+`=$1`, ownerID).Scan(&uid)
	if err == nil {
		return uid, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRowContext(ctx, `
SELECT g FROM generate_series($1::int, $2::int) AS g
WHERE NOT EXISTS (SELECT 1 FROM agent_uids WHERE uid=g)
ORDER BY g LIMIT 1`, AgentUIDPoolFirst, AgentUIDPoolLast).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrAgentUIDPoolExhausted
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_uids(uid, `+string(owner)+`) VALUES($1, $2)`, uid, ownerID); err != nil {
		return 0, fmt.Errorf("assign agent uid %d to %s %d: %w", uid, owner, ownerID, err)
	}
	return uid, tx.Commit()
}
