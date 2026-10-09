package server

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain acquires a PostgreSQL advisory lock (7337741002) for the entire
// server test suite so cross-package DELETE cleanup races with db/agent
// packages are avoided when running `go test ./...`.
func TestMain(m *testing.M) {
	dsn, _, err := db.DSN()
	if err != nil {
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	// 세션 advisory lock 은 그 연결이 끊기면 풀린다. os.Exit 는 defer 를 건너뛰므로
	// 풀기·닫기를 defer 로 두어도 돌지 않는다. 프로세스가 끝나 연결이 끊길 때 풀린다.
	os.Exit(m.Run())
}

// postgresPingTimeout 는 skip 판별용 연결 확인이 응답 없는 DB에 묶이지 않게 하는 상한이다.
const postgresPingTimeout = 5 * time.Second

// skipWithoutPostgres 는 DB 설정이 없거나 DB에 연결하지 못하면 테스트를 건너뛴다.
// 설정 판별과 연결 확인만 skip으로 돌리고, 그 뒤의 오류(스키마 적용 등)는
// 호출자가 그대로 t.Fatal 하도록 남긴다.
func skipWithoutPostgres(t *testing.T) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres not configured: %v", err)
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), postgresPingTimeout)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
}
