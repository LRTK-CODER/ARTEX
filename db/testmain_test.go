package db

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain acquires a PostgreSQL advisory lock (7337741002) for the entire
// db test suite. Packages agent and server hold the same lock, so parallel
// `go test ./...` runs serialize across packages on the shared dev DB and
// avoid cross-package cleanup races (e.g. DELETE FROM assets WHERE id > X
// from one package deleting assets created by another).
func TestMain(m *testing.M) {
	dsn, _, err := DSN()
	if err != nil {
		// No DB configured — tests that need PG will skip themselves.
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	// 세션 advisory lock은 그 연결이 끊기면 풀린다. os.Exit는 defer를 건너뛰므로
	// 풀기·닫기를 defer로 두어도 돌지 않는다. 프로세스가 끝나 연결이 끊길 때 풀린다.
	os.Exit(m.Run())
}
