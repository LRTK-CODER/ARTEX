package llmrec

import (
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain 은 db·agent·server·evidence 와 같은 PostgreSQL advisory lock(7337741002)을
// 잡는다. 이 패키지 테스트의 db.Open 이 스키마 DDL 을 다시 적용하는데, 다른 패키지
// 테스트와 겹치면 `go test ./...` 에서 그쪽 질의와 교착(deadlock)이 난다.
func TestMain(m *testing.M) {
	dsn, _, err := db.DSN()
	if err != nil {
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	os.Exit(m.Run())
}
