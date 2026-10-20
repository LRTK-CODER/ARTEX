package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// 이 파일은 사용자 지정 도구 실행기다.
// system=false인 tools 행을 kind로 나눈다. command(명령을 렌더링해 Bash 바탕의 run을 재사용),
// script(Python만. 임시 파일에 쓰고 stdin=파라미터 JSON + env TOOL_*, 설정한 인터프리터 사용),
// http(직접 요청, 프록시 설정 가능). 이 도구들은 트래픽·오케스트레이션 도구처럼 seed가 필요 없다
// (원래 tools 표에 있다). hostTools로 넣고 연결된 에이전트로 거른다.

// ---------- 사용자 지정 도구 CRUD ----------

type customToolReq struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Enabled     bool            `json:"enabled"`
	Kind        string          `json:"kind"` // command | script | http
	Exec        json.RawMessage `json:"exec"`
	Deferred    bool            `json:"deferred"`
}

var reToolKey = reAgentKey // 에이전트 key와 같은 규칙: 영문 소문자로 시작하고 영문 소문자·숫자·밑줄만 쓴다

func (s *Server) pgCreateCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req customToolReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if !reToolKey.MatchString(req.Key) {
		writeErr(w, 400, "key는 영문 소문자로 시작하고 영문 소문자, 숫자, 밑줄만 쓸 수 있습니다")
		return
	}
	if req.Kind != "command" && req.Kind != "script" && req.Kind != "http" && req.Kind != "shell" {
		writeErr(w, 400, "kind는 command, script, http, shell 중 하나여야 합니다")
		return
	}
	if req.Kind == "http" && !hasSchemaProps(req.Schema) {
		writeErr(w, 400, "http 도구는 파라미터 JSON Schema가 있어야 합니다(비워 둘 수 없습니다)")
		return
	}
	if exist, _ := pg.GetTool(req.Key); exist != nil {
		writeErr(w, 409, "같은 key의 도구(내장 또는 사용자 지정)가 이미 있습니다")
		return
	}
	if err := pg.CreateCustomTool(&db.Tool{
		Key: req.Key, Description: req.Description, Schema: req.Schema, Agents: req.Agents,
		Enabled: req.Enabled, Kind: req.Kind, Exec: req.Exec, Deferred: req.Deferred,
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"key": req.Key})
}

func (s *Server) pgUpdateCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	existing, err := pg.GetTool(key)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if existing == nil || existing.System {
		writeErr(w, 400, "사용자 지정 도구만 수정할 수 있습니다")
		return
	}
	var req customToolReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Kind != "command" && req.Kind != "script" && req.Kind != "http" && req.Kind != "shell" {
		writeErr(w, 400, "kind는 command, script, http, shell 중 하나여야 합니다")
		return
	}
	if req.Kind == "http" && !hasSchemaProps(req.Schema) {
		writeErr(w, 400, "http 도구는 파라미터 JSON Schema가 있어야 합니다(비워 둘 수 없습니다)")
		return
	}
	if err := pg.UpdateCustomTool(&db.Tool{
		Key: key, Description: req.Description, Schema: req.Schema, Agents: req.Agents,
		Enabled: req.Enabled, Kind: req.Kind, Exec: req.Exec, Deferred: req.Deferred,
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) pgDeleteCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	if err := pg.DeleteCustomTool(key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": key})
}

// testToolReq is a dry-run request from the editor: run the given (possibly unsaved)
// exec spec with sample params, without persisting the tool. Same executor path as a
// real tool call — it runs arbitrary command/script/http on the server, which the
// custom-tool feature already allows, so no new capability is granted.
type testToolReq struct {
	Kind   string          `json:"kind"` // command | script | http
	Exec   json.RawMessage `json:"exec"`
	Params map[string]any  `json:"params"`
}

// pgTestCustomTool executes an exec spec once and returns its raw output + error
// flag, so the editor can debug a tool before saving it. Per-kind timeouts still
// apply from the exec spec (with defaults); the outer ceiling is a hard backstop.
func (s *Server) pgTestCustomTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req testToolReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "잘못된 요청 본문입니다")
		return
	}
	params := req.Params
	if params == nil {
		params = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	ws, err := s.toolTestWorkspace()
	if err != nil {
		writeErr(w, 500, "시험 실행 작업 공간을 만들지 못했습니다: "+err.Error())
		return
	}
	defer func() {
		// 정리 실패는 시험 결과와 무관하다. 남은 디렉터리는 sessions/ 아래 이름으로 알아볼 수 있게 기록만 한다.
		if err := os.RemoveAll(ws); err != nil {
			log.Printf("[custom-tool] 시험 실행 작업 공간을 지우지 못했다: %v", err)
		}
	}()
	tc := &actool.ToolContext{WorkingDir: ws}
	var res actool.Result
	switch req.Kind {
	case "command":
		res, _ = s.runCommandTool(ctx, req.Exec, params, tc)
	case "script":
		res, _ = s.runScriptTool(ctx, "test", req.Exec, params, tc)
	case "http":
		res, _ = s.runHTTPTool(ctx, req.Exec, params, tc)
	case "shell":
		writeErr(w, 400, "shell 유형 도구는 bash 환경 안내라서 실행할 내용이 없습니다")
		return
	default:
		writeErr(w, 400, "알 수 없는 도구 유형: "+req.Kind)
		return
	}
	writeJSON(w, 200, map[string]any{"output": res.Flatten(), "is_error": res.IsError})
}

// toolTestWorkspaceDir 는 시험 실행 작업 공간을 두는 데이터 디렉터리 아래 디렉터리다. 대화 작업 공간과 같은 곳이라
// 에이전트 uid 분리(#165)를 켜도 에이전트 그룹이 쓸 수 있다.
const toolTestWorkspaceDir = "sessions"

// toolTestWorkspace 는 사용자 지정 도구 시험 실행에 쓸 빈 임시 작업 공간을 만든다. 실제 호출은 에이전트 작업
// 공간에서 돌므로 시험도 그와 같게 한다. 데이터 디렉터리를 작업 디렉터리로 쓰면 시험 명령이 데이터 디렉터리
// 전체를 보고, 기능을 켰을 때는 에이전트 uid가 그 안에 쓰거나 목록을 보지 못한다(데이터 디렉터리는 통과만 준다).
// 호출자가 끝나면 지운다.
func (s *Server) toolTestWorkspace() (string, error) {
	parent := filepath.Join(s.m.dir, toolTestWorkspaceDir)
	if err := os.MkdirAll(parent, 0o775); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "tool-test-")
	if err != nil {
		return "", err
	}
	if s.sandbox.SharesWorkspace() {
		// MkdirTemp는 0700으로 만든다. 그룹은 setgid인 부모에서 에이전트 그룹을 물려받는다. 부모는 에이전트가
		// 쓰지 못해 방금 만든 디렉터리를 바꿔치기할 수 없으므로 경로로 바꿔도 된다.
		if err := os.Chmod(dir, 0o770|os.ModeSetgid); err != nil {
			_ = os.RemoveAll(dir) // 빈 디렉터리다. 지우지 못해도 다음 시험에 영향이 없다
			return "", err
		}
	}
	return dir, nil
}

// ---------- Python 인터프리터(탐지 + 저장 + 덮어쓰기) ----------

const settingPythonInterp = "python_interpreter"

// detectPython 은 python 인터프리터(python3 우선)의 절대 경로를 찾는다. 에이전트 uid 분리를 켜면 서버 PATH에는
// 셸 대체만 있으므로 에이전트 명령의 PATH에서 찾는다(AgentSandbox.LookPath). 꺼져 있으면 서버 PATH다.
func (s *Server) detectPython() string {
	for _, c := range []string{"python3", "python"} {
		if p, err := s.sandbox.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// pythonInterpreter resolves the interpreter: user-set > stored auto-detect > live
// detect. "" only when truly none found.
func (s *Server) pythonInterpreter() string {
	if v, ok, _ := s.m.pg.GetSetting(settingPythonInterp); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return s.detectPython()
}

// seedPythonInterpreter stores the auto-detected interpreter on startup if unset
// (never clobbers a user-set value).
func (s *Server) seedPythonInterpreter() {
	if v, ok, _ := s.m.pg.GetSetting(settingPythonInterp); ok && strings.TrimSpace(v) != "" {
		return
	}
	if p := s.detectPython(); p != "" {
		_ = s.m.pg.SetSetting(settingPythonInterp, p)
		log.Printf("[custom-tool] python 인터프리터 자동 탐지: %s", p)
	}
}

// ---------- exec 명세 ----------

type commandExec struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms"`
}
type scriptExec struct {
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms"`
}
type httpExec struct {
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers"`
	Body              string            `json:"body"`
	TimeoutMs         int               `json:"timeout_ms"`
	Proxy             string            `json:"proxy"`
	UseRecordingProxy bool              `json:"use_recording_proxy"`
}

func timeoutOr(ms, def int) time.Duration {
	if ms <= 0 {
		return time.Duration(def) * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// ---------- 공통 도구 생성 ----------

// customTools builds CoreTools for every user-defined (system=false) tool row.
// shell-kind tools are environment hints only — they surface in the Bash tool
// description via ToolResolve and do NOT create callable tool entries here.
func (s *Server) customTools() ([]actool.CoreTool, error) {
	rows, err := s.m.pg.ListCustomTools()
	if err != nil {
		return nil, err
	}
	out := make([]actool.CoreTool, 0, len(rows))
	for _, t := range rows {
		if t.Kind == "shell" {
			continue // shell hints are handled by ToolResolve → Bash description
		}
		out = append(out, s.buildCustomTool(t))
	}
	return out, nil
}

// buildCustomTool 은 사용자 지정 도구 행 하나를 CoreTool로 바꾼다. 스키마가 비어 있으면 얇은
// {args:string} 래퍼 도구가 되어 command·http 템플릿이 {args}를 쓸 수 있다.
func (s *Server) buildCustomTool(t *db.Tool) actool.CoreTool {
	schema := ensureSchema(t.Schema)
	key, kind, execRaw := t.Key, t.Kind, t.Exec
	run := func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
		var params map[string]any
		if len(in) > 0 {
			_ = json.Unmarshal(in, &params)
		}
		if params == nil {
			params = map[string]any{}
		}
		switch kind {
		case "command":
			return s.runCommandTool(ctx, execRaw, params, tc)
		case "script":
			return s.runScriptTool(ctx, key, execRaw, params, tc)
		case "http":
			return s.runHTTPTool(ctx, execRaw, params, tc)
		default:
			return actool.Errorf("unknown custom tool kind: " + kind), nil
		}
	}
	return actool.Build(actool.Spec{
		Name: key, Description: t.Description, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: run,
	})
}

// hasSchemaProps reports whether raw is a JSON-Schema object with ≥1 property.
// http tools require an explicit schema (the auto {args} shell can't name the
// {param} placeholders in URL/headers/body), so an empty schema is rejected.
func hasSchemaProps(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	props, _ := m["properties"].(map[string]any)
	return len(props) > 0
}

// ensureSchema returns the tool's schema, or a thin {args:string} when none given.
func ensureSchema(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	props, _ := m["properties"].(map[string]any)
	if len(props) > 0 {
		return m
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{"type": "string", "description": "Command / arguments (free text)"},
		},
	}
}

// ---------- command: 명령 렌더링 → Bash 바탕의 run 재사용 ----------

func (s *Server) runCommandTool(ctx context.Context, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec commandExec
	_ = json.Unmarshal(execRaw, &spec)
	if strings.TrimSpace(spec.Command) == "" {
		return actool.Errorf("command is empty"), nil
	}
	cmd := renderTemplate(spec.Command, params, shellQuote)
	// Bash도 쓰는 바탕 run을 재사용한다(Bash CoreTool.Call 경유). 보안 기준선·시간 초과·프록시 env·
	// 출력 넘침 처리를 그대로 물려받는다. 이 도구는 Bash와 같은 층에서 바탕을 함께 쓸 뿐, 모델이
	// Bash 도구를 부르게 하지 않는다.
	bashIn, _ := json.Marshal(map[string]any{"command": cmd})
	if spec.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeoutOr(spec.TimeoutMs, 120000))
		defer cancel()
	}
	return actool.NewBash().Call(ctx, bashIn, tc)
}

// ---------- script(Python만): 임시 파일 + stdin JSON + env ----------

func (s *Server) runScriptTool(ctx context.Context, key string, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec scriptExec
	_ = json.Unmarshal(execRaw, &spec)
	if strings.TrimSpace(spec.Code) == "" {
		return actool.Errorf("script code is empty"), nil
	}
	interp := s.pythonInterpreter()
	if interp == "" {
		return actool.Errorf("no python interpreter configured or detected (set it in system settings)"), nil
	}
	workDir := s.m.dir
	var sessionEnv []string
	if tc != nil {
		if tc.WorkingDir != "" {
			workDir = tc.WorkingDir
		}
		sessionEnv = tc.Env
	}
	body, err := execPython(ctx, interp, key, spec.Code, params, workDir, sessionEnv, timeoutOr(spec.TimeoutMs, 120000), s.sandbox)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(actool.Capture(tc, body)), nil
}

// execPython writes the code to a temp .py under workDir/.tools, runs it via interp
// with the params JSON on stdin + scalar params mirrored to TOOL_<NAME> env, and
// returns combined stdout+stderr (with a timeout/exit note). Standalone + testable.
// sandbox가 켜져 있으면 도우미로 감싸 에이전트 uid로 실행한다.
func execPython(ctx context.Context, interp, key, code string, params map[string]any, workDir string, sessionEnv []string, timeout time.Duration, sandbox *AgentSandbox) (string, error) {
	tmp, cleanup, err := writeToolScript(workDir, key, code)
	if err != nil {
		return "", err
	}
	defer cleanup()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	name, args := sandbox.Command(interp, []string{tmp})
	c := exec.CommandContext(runCtx, name, args...)
	c.Dir = workDir
	c.Env = append(pythonToolEnviron(os.Environ()), sessionEnv...) // 세션 프록시 env
	for k, v := range sandbox.Env() {                              // 허용 목록이 거르므로 도우미가 읽을 uid·gid를 따로 넣는다
		c.Env = append(c.Env, k+"="+v)
	}
	for k, v := range params { // 스칼라 파라미터를 TOOL_<NAME>으로 복사한다
		if sv, ok := scalarStr(v); ok {
			c.Env = append(c.Env, "TOOL_"+strings.ToUpper(k)+"="+sv)
		}
	}
	pj, _ := json.Marshal(params)
	c.Stdin = bytes.NewReader(pj) // 파라미터 JSON은 stdin으로 넘긴다
	out, err := c.CombinedOutput()
	body := string(out)
	if runCtx.Err() == context.DeadlineExceeded {
		body += "\n... [terminated: timeout] ..."
	} else if err != nil {
		body += "\n[exit: " + err.Error() + "]"
	}
	return body, nil
}

// toolScriptDir 는 python 도구 코드를 두는 작업 디렉터리 아래 디렉터리다.
const toolScriptDir = ".tools"

// writeToolScript 는 code를 workDir/.tools 아래 새 파일에 쓰고 그 경로와 지우는 함수를 돌려준다.
//
// 작업 디렉터리 안에서는 os.Root로 만들고 지운다. 에이전트 uid 분리(#165)를 켜면 에이전트가 작업 공간에 .tools를
// 밖(키 디렉터리 등)을 가리키는 링크로 미리 둘 수 있다. 경로로 만들면 서버 uid가 그 링크를 따라 밖에 파일을 만들고
// 지운다. 작업 디렉터리 자체는 서버가 정한 경로이고 그 부모는 에이전트가 쓰지 못한다.
// 파일은 0640으로 만든다. uid 분리를 켜면 python이 에이전트 uid로 돌아 그룹 읽기가 있어야 연다. 끈 상태에서도
// 그룹(서버 그룹)에 읽기만 더하는 것이라 달라지는 것이 없다.
func writeToolScript(workDir, key, code string) (string, func(), error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", nil, err
	}
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return "", nil, err
	}
	if err := root.Mkdir(toolScriptDir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		root.Close()
		return "", nil, err
	}
	// 이름은 CreateTemp처럼 무작위로 짓고 O_EXCL로 만든다. 같은 이름이 있으면 실패한다.
	rel := filepath.Join(toolScriptDir, key+"-"+rand.Text()+".py")
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		root.Close()
		return "", nil, err
	}
	cleanup := func() {
		_ = root.Remove(rel) // 지우지 못하면 .tools에 스크립트가 남을 뿐이다
		root.Close()
	}
	_, err = f.WriteString(code)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return filepath.Join(workDir, rel), cleanup, nil
}

// 사용자 지정 python 도구가 서버 환경에서 물려받는 변수. 도구 코드는 LLM이 고른 인자로 돌고 대상 데이터를 다루므로,
// 비밀값이 아니더라도 서버 설정 변수는 넘기지 않는다. SystemRoot·TEMP·TMP·USERPROFILE은 Windows에서
// python이 시작하는 데 쓰인다(selfupdate 스모크 테스트 허용 목록과 같은 이유).
var (
	pythonToolEnvNames = []string{
		"PATH", "HOME", "LANG", "TERM", "TZ", "TMPDIR", "NODE_PATH", "SSL_CERT_FILE",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"SystemRoot", "TEMP", "TMP", "USERPROFILE",
	}
	pythonToolEnvPrefixes = []string{"LC_", "TOOL_"}
)

// pythonToolEnviron 은 environ에서 허용 목록의 변수만 남긴다. 프록시 변수는 소문자 이름(http_proxy)도 쓰이고
// Windows 이름은 대소문자를 가리지 않으므로 이름을 대소문자 구분 없이 비교한다.
func pythonToolEnviron(environ []string) []string {
	out := make([]string, 0, len(pythonToolEnvNames))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && isPythonToolEnvName(name) {
			out = append(out, kv)
		}
	}
	return out
}

func isPythonToolEnvName(name string) bool {
	for _, allowed := range pythonToolEnvNames {
		if strings.EqualFold(name, allowed) {
			return true
		}
	}
	upper := strings.ToUpper(name)
	for _, prefix := range pythonToolEnvPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// ---------- http: 직접 요청 + 프록시 ----------

func (s *Server) runHTTPTool(ctx context.Context, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec httpExec
	_ = json.Unmarshal(execRaw, &spec)
	method := strings.ToUpper(strings.TrimSpace(spec.Method))
	if method == "" {
		method = "GET"
	}
	rawURL := renderTemplate(spec.URL, params, identity)
	if strings.TrimSpace(rawURL) == "" {
		return actool.Errorf("http url is empty"), nil
	}
	var bodyReader io.Reader
	if spec.Body != "" {
		bodyReader = strings.NewReader(renderTemplate(spec.Body, params, identity))
	}
	runCtx, cancel := context.WithTimeout(ctx, timeoutOr(spec.TimeoutMs, 30000))
	defer cancel()
	req, err := http.NewRequestWithContext(runCtx, method, rawURL, bodyReader)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, renderTemplate(v, params, identity))
	}
	client := &http.Client{Timeout: timeoutOr(spec.TimeoutMs, 30000)}
	if tr := s.httpProxyTransport(spec); tr != nil {
		client.Transport = tr
	}
	resp, err := client.Do(req)
	if err != nil {
		return actool.Errorf("request failed: " + err.Error()), nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := map[string]any{"status": resp.StatusCode, "body": string(respBody)}
	b, _ := json.Marshal(out)
	return actool.Text(actool.Capture(tc, string(b))), nil
}

// httpProxyTransport builds a Transport for the http tool's proxy config, or nil
// (direct). use_recording_proxy routes through the recording proxy + trusts its CA.
func (s *Server) httpProxyTransport(spec httpExec) *http.Transport {
	proxyStr := strings.TrimSpace(spec.Proxy)
	var caFile string
	if spec.UseRecordingProxy {
		if addr := s.m.ProxyAddr(); addr != "" {
			proxyStr = "http://" + addr
			caFile = s.m.ProxyCACert()
		}
	}
	if proxyStr == "" {
		return nil
	}
	pu, err := url.Parse(proxyStr)
	if err != nil {
		return nil
	}
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	if caFile != "" {
		if pem, err := os.ReadFile(caFile); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
		}
	}
	return tr
}

// ---------- helpers ----------

func identity(s string) string { return s }

// shellQuote single-quotes a value for safe shell interpolation.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// renderTemplate replaces {name} placeholders with each param's rendered value.
func renderTemplate(tmpl string, params map[string]any, quote func(string) string) string {
	out := tmpl
	for k, v := range params {
		out = strings.ReplaceAll(out, "{"+k+"}", quote(valToStr(v)))
	}
	return out
}

// valToStr renders a param value: scalars as-is, arrays/objects as compact JSON.
func valToStr(v any) string {
	if sv, ok := scalarStr(v); ok {
		return sv
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// scalarStr returns (string, true) for scalar values, ("", false) for arrays/objects.
func scalarStr(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return fmt.Sprintf("%t", x), true
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x)), true
		}
		return fmt.Sprintf("%g", x), true
	case nil:
		return "", true
	default:
		return "", false
	}
}
