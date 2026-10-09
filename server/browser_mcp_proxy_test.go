package server

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 캡처 프록시가 붙은 이전 기본값 행은 시작 때 새 기본값으로 바뀌고,
// 이어서 syncBrowserMCPProxy 가 현재 프록시를 다시 붙인다(#85).
func TestNewManagerMigratesLegacyBrowserMCPAndKeepsProxy(t *testing.T) {
	const proxy = "http://127.0.0.1:9"
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v)", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { pg.Close() }) // 복원 Cleanup 보다 먼저 등록해 마지막에 닫히게 한다

	var oldCommand, oldArgs, oldEnv string
	if err := pg.QueryRow(`SELECT COALESCE(command,''), args::text, env::text FROM mcp_servers WHERE name='browser'`).Scan(&oldCommand, &oldArgs, &oldEnv); err != nil {
		t.Fatal(err)
	}
	oldProxy, hadProxy, err := pg.GetSetting(settingGlobalProxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pg.Exec(`UPDATE mcp_servers SET command=$1, args=$2, env=$3 WHERE name='browser'`, oldCommand, oldArgs, oldEnv); err != nil {
			t.Errorf("restore browser mcp: %v", err)
		}
		if hadProxy {
			_ = pg.SetSetting(settingGlobalProxy, oldProxy) // 복원 실패는 다음 테스트가 드러낸다
		} else {
			pg.Exec(`DELETE FROM settings WHERE key=$1`, settingGlobalProxy)
		}
	})

	if _, err := pg.Exec(`UPDATE mcp_servers SET command='npx', args=$1 WHERE name='browser'`,
		`["@playwright/mcp","--headless","--proxy-server","http://127.0.0.1:1"]`); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetSetting(settingGlobalProxy, proxy); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	var raw string
	if err := pg.QueryRow(`SELECT args::text FROM mcp_servers WHERE name='browser'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"@playwright/mcp", "--headless", "--browser", "chromium", "--proxy-server", proxy}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
}
