package db

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// BrowserMCPName 은 내장 Playwright MCP 행의 이름이다. server 가 트래픽 캡처
// 상태에 맞춰 이 행의 프록시 인자와 CA 환경 변수를 맞춘다.
const BrowserMCPName = "browser"

const browserMCPCommand = "npx"

// browserMCPDefaultArgs 는 새 설치의 기본 인자다. @playwright/mcp 의 기본 브라우저는
// chrome 채널인데 컨테이너 이미지에는 Playwright chromium 만 있으므로 chromium 을
// 명시한다(#85).
var browserMCPDefaultArgs = []string{"@playwright/mcp", "--headless", "--browser", "chromium"}

// legacyBrowserMCPArgs 는 #85 이전에 seed 하던 기본 인자다. 이 값 그대로 남은 행만
// 새 기본값으로 바꾼다.
var legacyBrowserMCPArgs = []string{"@playwright/mcp", "--headless"}

// seedBrowserMCP 는 내장 browser MCP 를 처음 한 번만 넣고(기본 비활성, 프록시 없음),
// 이전 기본값이 그대로 남은 기존 행을 새 기본값으로 바꾼다. 사용자가 고친 행은
// 건드리지 않는다. 시작할 때마다 돌아도 결과가 같다.
func (d *DB) seedBrowserMCP() error {
	defaultArgs, err := json.Marshal(browserMCPDefaultArgs)
	if err != nil {
		return fmt.Errorf("encode browser mcp args: %w", err)
	}
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ($1, 'stdio', $2, $3, '{}', false)
ON CONFLICT (name) DO NOTHING`, BrowserMCPName, browserMCPCommand, string(defaultArgs)); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}

	var command string
	var rawArgs []byte
	err = d.QueryRow(`SELECT COALESCE(command, ''), args FROM mcp_servers WHERE name = $1`, BrowserMCPName).Scan(&command, &rawArgs)
	if err != nil {
		return fmt.Errorf("read browser mcp: %w", err)
	}
	var args []string
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		// 문자열 배열이 아니면 사용자가 직접 바꾼 행이므로 그대로 둔다.
		return nil
	}
	if !isLegacyBrowserMCPDefault(command, args) {
		return nil
	}
	// 프록시 인자는 버린다. server 가 시작 직후 캡처 상태에 맞춰 다시 붙인다.
	// 읽은 뒤 행이 바뀌었으면 덮어쓰지 않도록 읽은 args 와 같을 때만 바꾼다.
	if _, err := d.Exec(`UPDATE mcp_servers SET args = $1 WHERE name = $2 AND args = $3::jsonb`,
		string(defaultArgs), BrowserMCPName, string(rawArgs)); err != nil {
		return fmt.Errorf("migrate browser mcp args: %w", err)
	}
	return nil
}

// isLegacyBrowserMCPDefault 는 행이 이전 기본값 그대로인지 본다. 시스템이 넣는
// 프록시 인자(StripBrowserProxyArgs)만 빼고 비교한다. 그 밖의 인자가 하나라도
// 다르면 사용자가 고친 것으로 본다.
func isLegacyBrowserMCPDefault(command string, args []string) bool {
	return command == browserMCPCommand && slices.Equal(StripBrowserProxyArgs(args), legacyBrowserMCPArgs)
}

// StripBrowserProxyArgs 는 트래픽 캡처가 browser MCP 에 넣는 --proxy-server/--proxy-bypass
// 인자("--flag 값"과 "--flag=값" 모두)를 뺀 새 슬라이스를 돌려준다. 입력은 바꾸지 않는다.
func StripBrowserProxyArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--proxy-server" || a == "--proxy-bypass" {
			i++ // 뒤따르는 값도 건너뛴다
			continue
		}
		if strings.HasPrefix(a, "--proxy-server=") || strings.HasPrefix(a, "--proxy-bypass=") {
			continue
		}
		out = append(out, a)
	}
	return out
}
