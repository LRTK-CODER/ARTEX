// Package db 는 ARTEX 의 PostgreSQL 데이터 소스다(이전의 graph 단일 파일 SQLite 를 대체한다).
// 연결을 열고, schema 를 적용하고, 내장 agent 와 변수 목록을 seed 한다.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver ("pgx")
)

//go:embed schema.sql
var schemaSQL string

const schemaMigrationLockKey int64 = 7337741001

var schemaDeadlockRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func applySchemaWithRetry(ctx context.Context, execer schemaExecer, sleep func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		if _, err := execer.ExecContext(ctx, schemaSQL); err != nil {
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			sleep(schemaDeadlockRetryDelays[attempt])
			continue
		}
		return nil
	}
}

// withSchemaMigrationLock pins the session-level lock to one checked-out
// connection. Running pg_advisory_lock through *sql.DB is incorrect because a
// later schema or unlock call may use a different pooled PostgreSQL session.
func withSchemaMigrationLock(ctx context.Context, sqlDB *sql.DB, action func(*sql.Conn) error) (err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
	}()
	return action(conn)
}

// coordinateWithSchemaMigration makes long, multi-table archive transactions
// mutually exclusive with startup DDL while allowing ordinary runtime queries
// to continue normally.
func coordinateWithSchemaMigration(tx *sql.Tx) error {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("coordinate with schema migration: %w", err)
	}
	return nil
}

// DSN resolves the PostgreSQL connection string and reports where it came from.
// Precedence: env ARTEX_PG_DSN > config file (config.json). There is no
// built-in default — it errors if neither source is configured.
func DSN() (dsn, source string, err error) {
	return config.PostgresDSN()
}

// DB wraps the shared *sql.DB. PG handles its own connection pool + concurrency
// (MVCC), so unlike the old SQLite store there is no process-wide write mutex.
type DB struct{ *sql.DB }

// ensureDatabase connects to the postgres system database and creates the target
// database if it does not exist. dsn must be a postgres:// URL.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // unparseable DSN — let the normal Open fail with a clear error
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || dbName == "postgres" {
		return nil
	}
	// connect to the postgres maintenance database instead
	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		return nil // best-effort; let Open surface the real error
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil
	}
	var exists bool
	_ = admin.QueryRow(`SELECT true FROM pg_database WHERE datname=$1`, dbName).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
			return fmt.Errorf("create database %q: %w", dbName, err)
		}
	}
	return nil
}

// Open connects, applies the schema (idempotent), and seeds builtin rows.
func Open(dsn string) (*DB, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", config.Redact(dsn), err)
	}
	d := &DB{sqlDB}
	// pgx runs multi-statement Exec via the simple protocol when there are no args.
	// Keep the dedicated lock connection checked out until both DDL and seeding
	// finish so concurrent application instances cannot initialize out of order.
	err = withSchemaMigrationLock(context.Background(), sqlDB, func(conn *sql.Conn) error {
		if err := applySchemaWithRetry(context.Background(), conn, time.Sleep); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := d.seedBuiltins(); err != nil {
			return fmt.Errorf("seed builtins: %w", err)
		}
		return nil
	})
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 행을 만들 때의 대화형 shell 기본값. ON CONFLICT 는 사용자가 나중에 바꾼 값을 덮어쓰지 않는다
	runSeconds            *int // 행을 만들 때의 run 1회 실제 경과 시간 상한(초). nil=seed 기본값(1200), 0=제한 없음
}

type promptVar struct{ name, desc, example, source string }

// intp 는 v 의 포인터를 돌려준다. builtinAgent 의 선택 필드(runSeconds 등)에 값을 명시할 때 쓴다.
func intp(v int) *int { return &v }

// builtinAgents 는 docs §5(a) 를 따른다. 내장 도구는 DB 에 저장하지 않고, 여기서는 agent 와 변수 목록만 seed 한다.
// planner/worker/mainagent/auto 의 대화형 shell 기본값은 아래 interactive_shell_default_v1
// 블록이 한꺼번에 true 로 둔다(나중의 토글을 존중한다). 여기의 interactiveShell 은 행을 만들 때부터 켜야 하는 새 agent 에만 쓴다.
var builtinAgents = []builtinAgent{
	{"goals", "목표 분해", "goals", "침투 테스트 작업 목표를 독립적이고 검증 가능한 하위 목표 여러 개로 나눕니다.", []promptVar{
		{"EngagementDescription", "작업 설명(테스트 대상/배경)", "example.com 사이트 테스트", "exploration"},
		// Now 는 전역 runtime 변수다(server.globalPromptVars 참고). 각 agent 목록에
		// 다시 정의하면 withGlobalVars 가 덧붙일 때 전역 항목과 이름이 겹친다.
	}, false, nil},
	{"planner", "플래너", "planner", "현황을 읽고 목표 달성 여부를 판정하며, 다루지 않은 새 방향이 있을 때만 탐색 의도를 더합니다(작업마다 계획 루프 하나).", []promptVar{
		{"Goal", "작업 전체 목표", "example.com 관리자 권한 획득", "exploration"},
		{"AssetSummary", "자산 수/유형 분포 요약(선택)", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "메인", "main", "사람과의 접점: 진행 상황을 지켜보고 사람의 의도를 힌트나 높은 우선순위 탐색 의도로 바꿉니다.", []promptVar{
		{"Goal", "현재 작업 목표", "example.com 관리자 권한 획득", "exploration"},
		{"AssetSummary", "시작 시점 현황 요약(선택)", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "확인된 취약점 요약(선택)", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "워커", "worker", "탐색 의도 하나를 할당받아 실행하고, 찾은 사실/취약점을 지식 그래프에 기록한 뒤 멈춥니다.", []promptVar{
		{"ProxyAddr", "기록 프록시 주소(if 분기 문구를 고른다)", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "워커 자기 식별 이름(선택)", "worker-1", "runtime"},
	}, false, nil},
	// Auto: 내장 "플랫폼 조작" agent. 침투 테스트 오케스트레이션 루프에 참여하지 않고, 대화 화면에서 구동되어 도구로 플랫폼을 조작한다.
	{"auto", "Auto", "assistant", "플랫폼 조작 도우미: 도구로 작업(생성/조회/일시 중지/힌트 주기)과 자산을 관리하고, 스킬·사용자 지정 도구·MCP를 만들거나 고칠 수 있습니다.", nil, false, nil},
	// 침투 테스트: 내장 "단독 침투 테스트" agent. 대화 화면에서 구동되어 정보 수집부터 마무리까지 침투 테스트 전 과정을 혼자 계획·실행·검증한다. 대화형 shell 이 기본으로 켜진다.
	{"pentest", "침투 테스트", "assistant", "단독 침투 테스트 agent: 정보 수집 → 공격 표면 찾기 → 익스플로잇 심화 → 검증 → 마무리의 전 과정을 혼자 계획하고 실행하며 적대적 관점으로 검증합니다.", nil, true, intp(0)},
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' 를 각 agent 목록에서 전역 runtime 변수로 올린 뒤에도 이전 DB 의 goals 에 'Now' 한 줄이 남아
	// 전역 항목과 이름이 겹친다(프런트 변수 목록 key 중복). 함께 지운다.
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// 내장 browser(Playwright) MCP. 트래픽 캡처 토글이 실행 중에 기록 프록시와 CA를
	// 넣고 뺀다(server.Manager.syncBrowserMCPProxy).
	if err := d.seedBrowserMCP(); err != nil {
		return err
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw `psql < schema.sql` init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultAssetInterceptRules(); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	if err := d.migrateBuiltinRuleTexts(); err != nil {
		return fmt.Errorf("migrate builtin rule texts: %w", err)
	}
	return nil
}

// seedDefaultAssetInterceptRules inserts the built-in asset blocklist (fuzzy
// domain matches for government / education sites) once on first startup. Gated
// by a settings flag so a user's later disable/delete is never resurrected on
// restart — same policy as the intercept-rule seed.
func (d *DB) seedDefaultAssetInterceptRules() error {
	if v, _, _ := d.GetSetting("asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := []struct {
		kind    string
		pattern string
		note    string
	}{
		{"fuzzy_domain", ".gov", "[내장] 정부 웹사이트 (.gov)"},
		{"fuzzy_domain", ".gov.cn", "[내장] 정부 웹사이트 (.gov.cn)"},
		{"fuzzy_domain", ".edu", "[내장] 교육 웹사이트 (.edu)"},
		{"fuzzy_domain", ".edu.cn", "[내장] 교육 웹사이트 (.edu.cn)"},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, $1, $2, $3, true)
ON CONFLICT DO NOTHING`, r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return d.SetSetting("asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here (e.g. playwright-cli, scopesentry) ship invisible by
// default — the user turns them on per-agent when needed. scopesentry additionally
// declares `mcps: ScopeSentry`, which only takes effect once it's made visible and
// that MCP is enabled/configured.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name | tool_input
		typ      string // string | regex
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── 시스템 파괴 명령 (priority 100) ──────────────────────────────────
		{
			name:     "[내장] 재귀 강제 삭제 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "재귀 강제 삭제(rm -rf / rm --recursive)를 금지합니다. 시스템이나 테스트 대상 환경을 영구히 손상시킬 수 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 시스템 핵심 디렉터리 삭제",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "시스템 핵심 경로 삭제를 금지합니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 포맷 mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "디스크 포맷(mkfs)을 금지합니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 장치 덮어쓰기 dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "dd로 디스크 장치를 덮어쓰는 것을 금지합니다",
			priority: 100,
		},
		{
			name:     "[내장] Fork 폭탄",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "Fork 폭탄 실행을 금지합니다",
			priority: 100,
		},
		{
			name:     "[내장] 시스템 종료 / 재부팅",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "시스템 종료·재부팅 명령 실행을 금지합니다",
			priority: 100,
		},
		{
			name:     "[내장] 모든 프로세스 강제 종료",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "kill -9 -1 또는 killall -9(모든 프로세스 강제 종료)를 금지합니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 완전 삭제 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "디스크 장치에 shred/wipe 완전 삭제를 실행하는 것을 금지합니다",
			priority: 100,
		},
		{
			name:     "[내장] 방화벽 규칙 비우기",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "방화벽 규칙 비우기(iptables -F / nft flush)를 금지합니다",
			priority: 100,
		},
		// ── 데이터베이스 파괴 동작 (priority 90) ─────────────────────────────────
		{
			name:     "[내장] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "DROP 실행을 금지합니다. 데이터베이스 객체를 되돌릴 수 없게 없앨 수 있습니다",
			priority: 90,
		},
		{
			name:     "[내장] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "TRUNCATE 실행을 금지합니다. 테이블의 모든 데이터를 비울 수 있습니다",
			priority: 90,
		},
		{
			name:     "[내장] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "MongoDB drop 실행을 금지합니다",
			priority: 90,
		},
		{
			name:     "[내장] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "Redis FLUSHALL / FLUSHDB 실행을 금지합니다. 캐시 데이터를 모두 비울 수 있습니다",
			priority: 90,
		},
		// ── HTTP 파괴 요청 (priority 80) ──────────────────────────────────
		// Agent 가 DELETE 요청을 보내는 흔한 세 가지 방식:
		//   1. curl -X DELETE / --request DELETE(Bash 도구로 바로 실행하거나 스크립트에 쓴다)
		//   2. Python HTTP 클라이언트의 .delete() 메서드
		//   3. JS/일반 스크립트의 method: 'DELETE' / method="DELETE"
		{
			name:     "[내장] curl / wget DELETE 요청",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "curl/wget으로 HTTP DELETE 요청을 보내는 것을 금지합니다. 대상 시스템의 데이터를 삭제할 수 있습니다",
			priority: 80,
		},
		{
			name:     "[내장] Python HTTP 클라이언트 DELETE(requests/httpx/aiohttp)",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "Python HTTP 클라이언트로 DELETE 요청을 보내는 것을 금지합니다",
			priority: 80,
		},
		{
			name:     "[내장] 스크립트의 HTTP DELETE 메서드 지정(JS/일반)",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "스크립트에서 HTTP DELETE 메서드를 지정해 요청을 보내는 것을 금지합니다",
			priority: 80,
		},
		{
			name:     "[내장] 일괄 비우기 / 파기 API 경로",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "일괄 비우기·파기 계열 API(/clear /wipe /flush /purge 등) 호출을 금지합니다",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 migrates the two safety patterns that used to be
// hard-coded in guard.go (destructive shell + data-exfil pipe) into ordinary
// intercept rules. Gated by its own flag so it also lands on DBs that already ran
// v1. Unlike the old guard.go floor, these are plain [내장] rules — the user can
// disable or delete them. The exfil rule ships DISABLED by default (its
// curl/wget/nc pipe pattern mis-fires on legitimate CTF/pentest reverse-shell and
// data-transfer pipes); enable it manually when exfil gating is actually wanted.
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[내장] 시스템 파괴 명령",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "파괴 명령을 거부했습니다(rm -rf / / mkfs / dd / fork bomb / 시스템 종료·재부팅 / 디스크 장치 덮어쓰기)",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[내장] 데이터 유출 파이프",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "데이터 유출로 의심되는 파이프를 거부했습니다(명령 출력을 curl/wget/nc로 외부에 보냄)",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3 adds the delete-endpoint path rule. The v1 HTTP rules
// only catch the DELETE *method* (curl -X DELETE, requests.delete(, method:'DELETE'),
// and v1's path rule covers only /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all — so a plain `curl 'http://t/api/user/delete?id=1'` (a
// delete endpoint reached with GET/POST, which is how most web apps expose deletion)
// slipped through every built-in rule. Own flag so it also lands on DBs that already
// ran v1/v2, where editing the v1 seed would have no effect.
//
// The pattern deliberately requires a separator after the verb so /delivery,
// /details, /delta and /delegate do not match, while /deleteAll, /delete_user and
// /delete-user do. destroy is re-covered here because v1's rule does not allow a
// suffix (/destroyAll was missed).
//
// Exported as a package const only so the seeded regex is unit-testable without a DB.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = "[내장] 삭제 API 경로"
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name = $1)`,
		name,
		deleteEndpointPathPattern,
		"삭제 계열 API(/delete /remove /unlink /erase 등) 호출을 금지합니다. HTTP 메서드와 관계없이 막습니다. 대부분의 애플리케이션은 GET/POST로도 삭제 API가 동작해 대상 데이터를 실제로 삭제합니다",
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}
