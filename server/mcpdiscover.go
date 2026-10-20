package server

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
	"github.com/Autumn-27/norma/mcp"
	actool "github.com/Autumn-27/norma/tool"
)

// mcpClient is the shared surface of a connected MCP server (stdio, Streamable HTTP,
// or legacy SSE),
// so tools/list and cleanup are handled uniformly regardless of transport.
type mcpClient interface {
	Tools(context.Context) ([]actool.CoreTool, error)
	Close() error
}

// connectMCP 는 전송 방식에 맞춰 MCP 서버 하나에 연결한다. 호출자가 Close해야 한다.
// sandbox가 켜져 있으면 stdio 서버를 도우미로 감싸 identity(AgentSandbox.RunEnv·SharedEnv)의 uid로 띄운다. MCP 서버는
// LLM이 고른 인자로 대상 데이터를 다루므로 그 실행의 에이전트 명령과 같은 경계에 둔다.
func connectMCP(ctx context.Context, m *db.MCPServer, sandbox *AgentSandbox, identity []string) (mcpClient, error) {
	switch m.Transport {
	case "stdio":
		if m.Command == "" {
			return nil, fmt.Errorf("stdio 전송 방식에 명령이 없습니다")
		}
		command, args := sandbox.Command(m.Command, jsonStrSlice(m.Args))
		return mcp.NewStdioClient(ctx, m.Name, command, stdioMCPEnv(jsonStrMap(m.Env), sandbox.Env(identity)), args...)
	case "http":
		if m.URL == "" {
			return nil, fmt.Errorf("http 전송 방식에 URL이 없습니다")
		}
		// env map doubles as HTTP headers (e.g. Authorization).
		return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	case "sse":
		if m.URL == "" {
			return nil, fmt.Errorf("sse 전송 방식에 URL이 없습니다")
		}
		return mcphttp.NewSSE(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	default:
		return nil, fmt.Errorf("알 수 없는 전송 방식 %q", m.Transport)
	}
}

// stdioMCPEnv 는 stdio MCP에 넘길 환경 변수다. 도우미가 읽는 값(extra, AgentSandbox.Env)은 사용자가 MCP 설정에 같은
// 이름을 적어도 서버 값이 이기도록 마지막에 덮어쓴다. 사용자가 적은 uid·gid는 먼저 뺀다. 서버가 실행의 uid를 싣지
// 못했을 때 설정의 값으로 채워져 돌지 않고 도우미가 거부하게 한다(#177). 꺼져 있으면(extra가 빔) 설정 그대로다.
func stdioMCPEnv(env, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(extra))
	for k, v := range env {
		if k == AgentUIDEnv || k == AgentGIDEnv {
			continue
		}
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// discoverAndCacheMCP connects to one MCP, lists its tools, and persists the tool
// names to mcp_tools_cache so the UI shows them without a live connection.
func (s *Server) discoverAndCacheMCP(ctx context.Context, m *db.MCPServer) error {
	// 설정 화면의 도구 목록 조회는 작업·세션에 속하지 않으므로 공용 에이전트 uid로 띄운다.
	cl, err := connectMCP(ctx, m, s.sandbox, s.sandbox.SharedEnv())
	if err != nil {
		return err
	}
	defer cl.Close()
	ts, err := cl.Tools(ctx)
	if err != nil {
		return err
	}
	tools := make([]db.MCPTool, 0, len(ts))
	for _, t := range ts {
		tools = append(tools, db.MCPTool{Name: t.Name(), Description: t.Description()})
	}
	if err := s.m.pg.SaveMCPTools(m.ID, tools); err != nil {
		return err
	}
	log.Printf("[mcp] %s: 도구 %d개를 찾아 캐시함", m.Name, len(tools))
	return nil
}

// discoverEmptyMCPsOnStartup fills the tool cache for any enabled MCP that has none
// yet (notably the seeded browser MCP on first run). Runs sequentially in one
// goroutine so we never spawn many stdio servers (npx) at once, and never blocks
// startup. Best-effort: a failure leaves the cache empty to retry next start.
func (s *Server) discoverEmptyMCPsOnStartup() {
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		log.Printf("[mcp] 시작 시 자동 탐지: 목록 읽기 실패: %v", err)
		return
	}
	for _, m := range servers {
		if !m.Enabled || len(m.Tools) > 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
		if err := s.discoverAndCacheMCP(ctx, m); err != nil {
			log.Printf("[mcp] 시작 시 자동 탐지 %s 실패: %v", m.Name, err)
		}
		cancel()
	}
}
