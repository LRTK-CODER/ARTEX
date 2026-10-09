package evidence

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// 명시적으로 준 DB 를 db·agent·server·llmrec 와 같은 suite lock 아래에서 초기화한다.
// lock 은 고정한 연결 하나에서 잡는다.
func TestMain(m *testing.M) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		os.Exit(m.Run())
	}
	os.Exit(runEvidenceSuite(m))
}

func runEvidenceSuite(m *testing.M) int {
	ctx := context.Background()
	dsn := os.Getenv("ARTEX_PG_DSN")
	lockDB, err := sql.Open("pgx", dsn)
	if err != nil {
		panic(err)
	}
	defer lockDB.Close()
	conn, err := lockDB.Conn(ctx)
	if err != nil {
		// 데이터베이스가 아직 없으면 lock 을 잡을 DB 가 없으므로 db.Open 으로 먼저 만든다.
		// 이 단계는 lock 밖이다. `go test ./...` 에서 다른 패키지(db·agent·server·llmrec)도
		// 같은 순간 DB 를 만들거나 스키마를 적용할 수 있어 겹칠 상대가 없다고 장담할 수 없다.
		pg, openErr := db.Open(dsn)
		if openErr != nil {
			panic(openErr)
		}
		pg.Close()
		if conn, err = lockDB.Conn(ctx); err != nil {
			panic(err)
		}
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(7337741002)`); err != nil {
		panic(err)
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock(7337741002)`)
	// db.Open 의 스키마 적용은 테이블에 AccessExclusiveLock 을 거는 DDL 이다. lock 없이 하면
	// lock 을 쥔 다른 패키지 테스트의 질의와 교착(deadlock)이 나므로 lock 을 잡은 뒤에 한다.
	pg, err := db.Open(dsn)
	if err != nil {
		panic(err)
	}
	defer pg.Close()
	return m.Run()
}
