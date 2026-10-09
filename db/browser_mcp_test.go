package db

import (
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
)

// restoreBrowserMCPRow 는 공유 개발 DB 의 browser 행을 테스트 전 상태로 되돌린다.
func restoreBrowserMCPRow(t *testing.T, d *DB) {
	t.Helper()
	var command sql.NullString
	var args, env []byte
	var enabled bool
	err := d.QueryRow(`SELECT command, args, env, enabled FROM mcp_servers WHERE name='browser'`).Scan(&command, &args, &env, &enabled)
	if err == sql.ErrNoRows {
		t.Cleanup(func() { d.Exec(`DELETE FROM mcp_servers WHERE name='browser'`) })
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', $1, $2, $3, $4)
ON CONFLICT (name) DO UPDATE SET command=EXCLUDED.command, args=EXCLUDED.args, env=EXCLUDED.env, enabled=EXCLUDED.enabled`,
			command, string(args), string(env), enabled); err != nil {
			t.Errorf("restore browser mcp: %v", err)
		}
	})
}

func browserMCPArgs(t *testing.T, d *DB) []string {
	t.Helper()
	var raw []byte
	if err := d.QueryRow(`SELECT args FROM mcp_servers WHERE name='browser'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("args %s: %v", raw, err)
	}
	return args
}

// reopen 은 seed 를 다시 돌리려고 같은 DB 를 한 번 더 연다(재시작과 같다).
func reopen(t *testing.T) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	d.Close()
}

func TestSeedBrowserMCPNewInstallUsesChromium(t *testing.T) {
	d := openTestDB(t)
	restoreBrowserMCPRow(t, d)
	if _, err := d.Exec(`DELETE FROM mcp_servers WHERE name='browser'`); err != nil {
		t.Fatal(err)
	}
	reopen(t)

	want := []string{"@playwright/mcp", "--headless", "--browser", "chromium"}
	if got := browserMCPArgs(t, d); !slices.Equal(got, want) {
		t.Fatalf("새 설치 args = %q, want %q", got, want)
	}
	var enabled bool
	if err := d.QueryRow(`SELECT enabled FROM mcp_servers WHERE name='browser'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("내장 browser MCP 는 기본 비활성이어야 한다")
	}
}

func TestSeedBrowserMCPMigratesUnmodifiedLegacyDefault(t *testing.T) {
	newDefault := []string{"@playwright/mcp", "--headless", "--browser", "chromium"}
	cases := []struct {
		name    string
		command string
		args    string
		want    []string
	}{
		{
			name:    "이전 기본값 그대로",
			command: "npx",
			args:    `["@playwright/mcp","--headless"]`,
			want:    newDefault,
		},
		{
			name:    "캡처가 붙인 프록시 인자가 있는 이전 기본값",
			command: "npx",
			args:    `["@playwright/mcp","--headless","--proxy-server","127.0.0.1:8788"]`,
			want:    newDefault,
		},
		{
			name:    "= 형식 프록시 인자가 있는 이전 기본값",
			command: "npx",
			args:    `["@playwright/mcp","--headless","--proxy-server=127.0.0.1:8788","--proxy-bypass","localhost"]`,
			want:    newDefault,
		},
		{
			name:    "사용자가 인자를 더한 행",
			command: "npx",
			args:    `["@playwright/mcp","--headless","--isolated"]`,
			want:    []string{"@playwright/mcp", "--headless", "--isolated"},
		},
		{
			name:    "사용자가 브라우저를 고른 행",
			command: "npx",
			args:    `["@playwright/mcp","--headless","--browser","firefox"]`,
			want:    []string{"@playwright/mcp", "--headless", "--browser", "firefox"},
		},
		{
			name:    "사용자가 명령을 바꾼 행",
			command: "/usr/local/bin/npx",
			args:    `["@playwright/mcp","--headless"]`,
			want:    []string{"@playwright/mcp", "--headless"},
		},
		{
			name:    "사용자가 headless 를 뺀 행",
			command: "npx",
			args:    `["@playwright/mcp"]`,
			want:    []string{"@playwright/mcp"},
		},
	}
	d := openTestDB(t)
	restoreBrowserMCPRow(t, d)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', $1, $2, '{}', false)
ON CONFLICT (name) DO UPDATE SET command=EXCLUDED.command, args=EXCLUDED.args`, tc.command, tc.args); err != nil {
				t.Fatal(err)
			}
			reopen(t)
			if got := browserMCPArgs(t, d); !slices.Equal(got, tc.want) {
				t.Fatalf("args = %q, want %q", got, tc.want)
			}
			// 다시 돌려도 같은 결과여야 한다.
			reopen(t)
			if got := browserMCPArgs(t, d); !slices.Equal(got, tc.want) {
				t.Fatalf("두 번째 시작 뒤 args = %q, want %q", got, tc.want)
			}
		})
	}
}
