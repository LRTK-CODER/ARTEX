package traffic

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// openLegacyIndex builds the index exactly as the pre-reclamation Open did: a
// plain-path DSN, pragmas via the pool, and auto_vacuum left at its default 0.
func openLegacyIndex(t *testing.T, dir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "_index"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(dir, "_index", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := old.Exec(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(indexSchema); err != nil {
		t.Fatal(err)
	}
	return old
}

// TestUpgradeFromOldInstall guards the upgrade path. Open now names the database
// through a file: URI so per-connection pragmas can ride in the DSN, and a
// driver that did not treat that as a URI would quietly open a file literally
// named "file:/…" — an empty index, with every recorded exchange apparently
// gone. The assertions below are what prove that does not happen.
func TestUpgradeFromOldInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_index", "index.sqlite")
	old := openLegacyIndex(t, dir)
	if _, err := old.Exec(ftsSchema); err != nil {
		t.Fatal(err)
	}
	// 과거 트래픽 3건, 그중 하나는 legacy path<>'' 행이다
	for i, row := range [][]any{
		{"1700000000-0001", "old.example.com", ""},
		{"1700000000-0002", "old.example.com", ""},
		{"1700000000-0003", "legacy.example.com", "legacy.example.com/GET/x"},
	} {
		if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,'GET','/x','http://x/x',200,'text/html',0,9,?)`, row[0], 1700000000+i, row[1], row[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,resp_head,resp_body)
VALUES(?,'GET /x','','HTTP 200','老数据正文')`, row[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO ex_fts(rowid,content) VALUES(?,?)`, i+1, "老数据正文 secret-token"); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// ---- 새 버전이 넘겨받는다
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("새 버전이 이전 저장소를 열지 못한다: %v", err)
	}
	defer tr.Close()

	// 1. 반드시 같은 파일이어야 하고, 몰래 빈 저장소를 새로 열어서는 안 된다
	if st2, err := os.Stat(path); err != nil || st2.Size() == 0 {
		t.Fatalf("원래 색인 파일이 이상하다: size=%v err=%v", st2, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "_index")); len(entries) > 3 {
		for _, e := range entries {
			t.Logf("_index 아래: %s", e.Name())
		}
		t.Fatal("_index 아래에 예상 밖의 파일이 나타났다. DSN이 다른 저장소를 가리켰을 수 있다")
	}
	t.Logf("이전 저장소 %d바이트, 새 버전이 넘겨받은 뒤에도 같은 파일이다", stat.Size())

	// 2. 과거 데이터가 모두 보인다
	n, err := tr.Count()
	if err != nil || n != 3 {
		t.Fatalf("Count = (%d,%v), 기대값 (3,nil) — 과거 트래픽이 사라졌다", n, err)
	}
	// 3. 과거 전문 색인을 여전히 검색할 수 있다
	if tr.fts {
		rows, err := tr.query("old.example.com", "", "secret-token", 0, 10)
		if err != nil {
			t.Fatalf("과거 전문 검색 실패: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("과거 전문 검색 결과 %d건, 기대값 2", len(rows))
		}
	}
	// 4. 과거 본문을 여전히 읽을 수 있다
	if _, resp, err := tr.Get("1700000000-0001"); err != nil {
		t.Fatalf("과거 본문 읽기 실패: %v", err)
	} else if resp == "" {
		t.Fatal("과거 응답이 비어 있다")
	}
	// 5. 이전 저장소가 증분 회수 사용으로 잘못 판정되지 않는다
	if tr.incrementalVacuum {
		t.Fatal("이전 저장소가 증분 회수 사용으로 잘못 판정됐다")
	}
	// 6. 삭제가 여전히 정상 동작하고, 회수 과정이 이전 저장소에서 수렴한다
	deleted, err := tr.DeleteHostsExact([]string{"old.example.com"})
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteHostsExact = (%d,%v), 기대값 (2,nil)", deleted, err)
	}
	tr.reaping.Wait()
	if n, err := tr.Count(); err != nil || n != 1 {
		t.Fatalf("삭제 뒤 Count = (%d,%v), 기대값 (1,nil)", n, err)
	}
	// 7. legacy path<>'' 행은 영향을 받지 않았다
	var legacyPath string
	if err := tr.DB().QueryRow(`SELECT path FROM exchanges`).Scan(&legacyPath); err != nil {
		t.Fatal(err)
	}
	if legacyPath == "" {
		t.Fatal("legacy 행의 path가 비워졌다")
	}
}

// TestDowngradeToOldBinary covers a rollback: a database created with
// auto_vacuum=incremental must stay readable and writable by a build that knows
// nothing about it. auto_vacuum only changes where SQLite tracks free pages, so
// the old binary simply goes back to never returning them.
func TestDowngradeToOldBinary(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if !tr.incrementalVacuum {
		t.Fatal("새 저장소는 증분 회수를 사용해야 한다")
	}
	bulkRecord(tr, "keep.example.com", 5, 100*1024)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	old := openLegacyIndex(t, dir) // 이전 버전 바이너리가 넘겨받는다
	defer old.Close()
	var n int
	if err := old.QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("이전 버전이 읽은 값 (%d,%v), 기대값 (5,nil)", n, err)
	}
	if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('x',1,'new.example.com','GET','/x','http://x/x',200,'',0,0,'')`); err != nil {
		t.Fatalf("이전 버전 쓰기 실패: %v", err)
	}
	if _, err := old.Exec(`DELETE FROM exchanges WHERE host='keep.example.com'`); err != nil {
		t.Fatalf("이전 버전 삭제 실패: %v", err)
	}
}
