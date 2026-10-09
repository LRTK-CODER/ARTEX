// ARTEX 도메인 모델: UI 전반에서 쓰는 타입.
// 기능 명세(7절: 주요 데이터 형태)에서 가져왔다.

export type TaskStatus = "created" | "queued" | "running" | "paused" | "done" | "failed" | "timeout";
export type EngineMode = "exploring" | "paused" | "stalled" | "idle";

export interface Task {
  id: string;
  name?: string; // 선택 작업 이름. 비었거나 없으면 이름 없음이고, 표시할 때 설명으로 대신한다
  category_id?: number;
  category_name?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  description: string;
  goal: string;
  status: TaskStatus;
  created_at: string;
  created_unix?: number; // created_at as unix seconds (run-duration calc)
  completed_at?: string; // RFC3339 finish time (done/failed); "" if unfinished
  completed_unix?: number; // completed_at as unix seconds (0/undef if unfinished)
  last_activity_unix?: number; // unix seconds of the last activity (0/undef if none)
  paused?: boolean;
  queued?: boolean;
  active?: boolean;
  in_flight?: number;
  findings?: { critical: number; high: number; medium: number; low: number }; // 등록된 취약점 수(심각도별)
  last_activity?: string;
  stalled?: boolean;
  goals_total?: number;
  goals_met?: number;
  engine_mode?: EngineMode;
  tokens?: TokenTotal; // whole-task token consumption
  llm_profile_id?: number; // LLM profile used; absent = default profile
  llm_profile_ids?: number[]; // ordered task-level failover chain
  active_llm_profile_id?: number; // profile used by the next LLM call
  llm_failover_state?: "default" | "ready" | "chain_exhausted" | string;
  llm_failover_reason?: string;
  source_task_ids?: string[]; // directly related tasks inherited as read-only context
  archive_blocked_by_task_id?: string; // live direct dependent that must be archived first
  company_ids?: number[]; // associated company scopes; current company assets join the task at creation
  coverage_enabled?: boolean; // 자산 커버리지 기능 스위치(만들 때 정하며 기본값 켜짐). false면 커버리지를 계산하지도 보여 주지도 않는다
}

export interface TaskCategory {
  id: number;
  name: string;
  task_count: number;
  created_at: string;
  updated_at: string;
}

export interface TaskTemplate {
  id: number;
  name: string;
  description: string;
  goal: string;
  category_id?: number | null; // 미리 정한 분류. null이거나 없으면 분류 없음
  intercept_rules?: AssetInterceptRuleInput[]; // 미리 정한 작업 단위 차단·허용 규칙
  created_at: string;
  updated_at: string;
}

export interface DeleteTaskOptions {
  delete_assets: boolean;
  delete_traffic: boolean;
  delete_files: boolean;
  delete_findings: boolean;
  delete_llm_records: boolean;
}

export interface DeleteTaskResult {
  deleted: string;
  assets_deleted: number;
  assets_detached: number;
  traffic_deleted: number;
  files_deleted: boolean;
  findings_deleted: number;
  llm_records_deleted: number;
  cleanup_warning?: string;
}

export type TaskArchiveState =
  | "archive_queued"
  | "archiving"
  | "archive_failed"
  | "ready"
  | "restore_queued"
  | "restoring"
  | "restore_failed"
  | "delete_queued"
  | "deleting"
  | "delete_failed";

export interface TaskArchiveTokenStats {
  calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface TaskArchive {
  id: number;
  task_id: number;
  state: TaskArchiveState;
  phase: string;
  progress: number;
  error?: string;
  warnings?: string[];
  format_version: number;
  sha256?: string;
  original_size: number;
  compressed_size: number;
  task_name: string;
  task_description: string;
  task_goal: string;
  original_status: TaskStatus;
  category_id?: number;
  category_name?: string;
  source_task_ids: number[];
  remaining_timeout_seconds: number;
  data_counts: Record<string, number>;
  aggregate_stats: {
    tokens?: TaskArchiveTokenStats;
    skills?: Record<string, number>;
    tools?: Record<string, number>;
    findings?: Record<string, number>;
  };
  archived_at?: string;
  requested_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskArchivePage {
  items: TaskArchive[];
  total: number;
  page: number;
  size: number;
}

export interface ArchiveBatchItem {
  id: string;
  archive_id?: number;
  ok: boolean;
  queued: boolean;
  error?: string;
}

// ---- Asset graph (global, shared across tasks) ----
export type AssetType =
  | "company"
  | "domain"
  | "ip"
  | "port"
  | "service"
  | "site"
  | "endpoint"
  | "parameter"
  | "tech"
  | "credential"
  | "data";

export type NodeState = "observed" | "confirmed" | "tombstoned";

export interface AssetNode {
  id: string;
  type: AssetType;
  name: string;
  key: string; // nkey
  value?: string;
  company_id?: string; // 소속 기업 자산 id. 비면 소속 없음
  state: NodeState;
  confidence: number; // 0..1
  attrs?: Record<string, unknown>;
  first_seen: string;
  last_seen: string;
}

export type AssetRel =
  | "owns"
  | "resolves"
  | "exposes"
  | "runs"
  | "serves"
  | "has_endpoint"
  | "has_param"
  | "fingerprinted"
  | "authenticates_as"
  | "reachable"
  | "has_subdomain";

export interface Edge {
  src: string;
  dst: string;
  rel: AssetRel | ExploreRel;
}

// Task asset view — server-side enriched, paginated.
export interface TaskAssetRef {
  id: string;
  name?: string;
  key: string;
  attrs?: Record<string, unknown>;
}

export interface TaskAssetItem extends AssetNode {
  techs?: TaskAssetRef[];
  auth?: TaskAssetRef[];
  params?: TaskAssetRef[];
}

export interface TaskAssetView {
  counts: Record<string, number>;
  total: number;
  items: TaskAssetItem[];
}

// ---- New unified asset model (new backend) ----
export type NewAssetType = "root_domain" | "ip" | "subdomain" | "app" | "service" | "endpoint";

export interface Asset {
  id: number;
  type: NewAssetType;
  company_id?: number;
  task_ids: number[];
  domain?: string;
  root_domain?: string;
  ip?: string;
  c_segment?: string;
  port?: number;
  icp?: string;
  bound_domains?: string[];
  open_ports?: { port: number; service?: string }[];
  record_type?: string;
  record_value?: string[] | string;
  bundle_id?: string;
  app_name?: string;
  category?: string;
  app_description?: string;
  app_icp?: string;
  url?: string;
  service_type?: string;
  service_name?: string;
  favicon_mmh3?: string;
  status_code?: number;
  content_length?: number;
  page_title?: string;
  technologies?: string[];
  auth?: Record<string, unknown>[];
  method?: string;
  params?: Record<string, unknown>[];
  extra?: Record<string, unknown>;
  last_seen: string;
  task_source?: string;
  task_source_summary?: string;
  task_source_node_id?: number;
}

export interface IntentAsset {
  intent_id: number | string;
  asset_id: number;
  type: NewAssetType;
  label: string;
  source: string;
  source_summary: string;
  source_node_id?: number;
  source_task_id: number;
  inherited: boolean;
}

export interface TaskAssetMutation {
  requested: number;
  attached: number;
  existing: number;
}

export interface TaskAssetScopeMutation {
  requested: number;
  assets_linked: number;
  assets_existing: number;
  scopes_added: number;
  scopes_existing: number;
}

// ---- Asset coverage graph (per task) ----
// 포스 기반(force-directed) 배치 '자산 커버리지 그래프'의 노드 하나. key는 고유하다: 자산="a:<id>", 기업="c:<id>",
// 자산 행이 없는 루트 도메인="r:<domain>". in_scope=false는 연결선용으로만 쓰는 회색 컨텍스트 노드다.
export interface CoverageGraphNode {
  key: string;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint";
  label: string;
  tested: boolean;
  in_scope: boolean;
  asset_id?: number;
  company_id?: number;
  domain?: string;
  root_domain?: string;
  ip?: string;
  url?: string;
  port?: number;
  service_type?: string;
  app_name?: string;
  page_title?: string;
  status_code?: number;
}

export interface CoverageGraphEdge {
  src: string;
  dst: string;
}

export interface CoverageGraphData {
  nodes: CoverageGraphNode[];
  edges: CoverageGraphEdge[];
}

// 어떤 자산이 이 작업의 탐색 그래프에서 연결된 탐색 의도·사실·발견 사항(커버리지 그래프 노드 서랍에서 쓴다).
export interface CoverageAssetRef {
  id: number;
  kind: string;
  state: string;
  summary: string;
  source_task_id?: string;
  inherited?: boolean;
}
export interface CoverageAssetRefs {
  intents: CoverageAssetRef[];
  facts: CoverageAssetRef[];
  findings: CoverageAssetRef[];
}

// ---- Workspace file manager (workDir) ----
export interface WorkspaceEntry {
  name: string;
  path: string; // workspace-relative, forward slashes
  dir: boolean;
  size: number;
  mtime: number; // unix millis
}
export interface WorkspaceListing {
  path: string;
  entries: WorkspaceEntry[];
}
export interface WorkspaceFile {
  path: string;
  size: number;
  binary: boolean;
  too_large?: boolean;
  content?: string;
}

// 작업 테스트 범위의 한 항목(커버리지 분모이자 허가된 테스트 범위).
export interface TaskScopeRow {
  id: number;
  task_id: number;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "cidr" | "icp" | "keyword";
  company_id?: number;
  company_name?: string; // 백엔드가 companies를 JOIN해 채우며, kind=company일 때만 값이 있다
  domain?: string;
  net?: string;
  value?: string;
  source: "auto" | "agent" | "manual";
  reason?: string;
}

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 기업을 추가할 때 제출하는 구조화된 자산 범위 규칙.
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// 자산 범위를 쓴 결과. errors는 이번 제출에서 잘못된 행이다. warnings는 이번 제출과 관계없지만
// 소속 결과를 예상과 다르게 만드는 기존 데이터 문제다(예: ip 필드에 호스트 이름이 저장된 자산).
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 기업 자산 범위 규칙의 한 항목(소속을 정하는 유일한 기준).
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain일 때 값이 있다
  net?: string; // kind=ip|cidr일 때 값이 있다
  value?: string; // kind=icp|keyword일 때 백엔드가 바로 돌려줄 수 있다
  raw: string; // 사용자가 입력한 원래 값. 표시하고 입력 칸에 다시 채울 때 쓴다
  reason?: string;
}

// 기업: type=company인 자산 노드 + 아이콘 + 자산 수 + 자산 범위 규칙.
export interface Company {
  id: number;
  name: string;
  logo?: string; // 원격 아이콘 URL. 비면 프런트가 이름 첫 글자를 쓴다
  asset_count: number;
  scope?: ScopeRow[];
}

// ---- Exploration graph (per task) ----
export type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";
export type GoalState = "open" | "met" | "abandoned";
export type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";
export type FindingState = "confirmed" | "dismissed";
export type HintState = "active" | "consumed";
export type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // 탐색 의도를 소프트 삭제(state='deleted')할 때의 삭제 이유
}

// 활동 피드 한 페이지: 만든 순서로 나눈 노드 + 이 페이지에 걸린 간선 + 간선 반대쪽 노드(refs, id로 찾는다).
// 그래프 전체를 내려받지 않고도 피드 항목마다 '어디서 왔고 무엇을 만들어 냈는지' 보여 줄 수 있다.
export interface ExplorationNodePage {
  items: TaskNode[];
  total: number;
  page: number;
  size: number;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  // 노드 id → 그 노드에 연결된 자산(활동 피드를 펼칠 때 함께 보여 주며, 이 페이지 노드와 그 이웃을 포함한다).
  assets: Record<string, FindingAsset[]>;
}

export interface ExplorationNodeQuery {
  page?: number;
  size?: number;
  kinds?: ExploreKind[];
  states?: string[];
  q?: string;
  order?: "asc" | "desc";
}

// 목표 관리 카드에서 쓰는 목표(백엔드가 payload를 text/vulnclass로 이미 나눴다).
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState
  origin?: string;
  ts: string;
}

// 제약 조건 관리 카드에서 쓰는 작업 제약 조건(allow=허용 / deny=금지).
export type ConstraintKind = "allow" | "deny";
export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}

// ---- Findings ----
export type Severity = "critical" | "high" | "medium" | "low";

// 취약점 처리 상태: 처리 대기 / 처리 중 / 확인됨 / 처리됨 / 수정됨 / 오탐 / 무시 / 중복 / 위험 수용.
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset은 취약점 하나에 연결된 자산이다(label은 백엔드가 미리 만든다).
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 별도 findings 테이블의 행 id. 상태를 바꿀 때 쓰는 핸들이다(작업 안의 옛 노드에는 없을 수 있다)
  vulnclass: string;
  name?: string; // 취약점 이름. 비면 vulnclass로 대신 표시한다
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 상세 보고서(Markdown). 상세 API만 돌려주고 목록에서는 비어 있다
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage는 발견 사항 목록의 서버 쪽 페이지 응답이다.
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

export interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 선택 작업 이름. 비었거나 없으면 이름 없음
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats는 발견 사항 전체 집계다(통계 카드 + 취약점 유형 드롭다운). 서버가 계산하며 페이지 나누기의 영향을 받지 않는다.
export interface FindingStats {
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption은 발견 사항 페이지 '작업별' 필터 드롭다운의 한 항목이다: 취약점이 있는 작업(설명이 비면 작업이 삭제됐다는 뜻이며
// 프런트는 id를 대신 보여 준다)과 그 취약점 건수.
export interface FindingTaskOption {
  id: string | number;
  name?: string; // 선택 작업 이름. 비었거나 없으면 이름 없음
  description: string;
  count: number;
}

// FindingQuery는 발견 사항 목록의 페이지·필터·정렬 파라미터다.
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // 작업 id. "all"이거나 비면 작업으로 거르지 않는다
  query?: string;
  sort?: "severity" | "time";
  // 자산 트리 노드 key. 노드 하나를 고르면 그 하위 트리 전체를 고른 것이다. 비면 자산으로 거르지 않는다.
  assetScope?: string;
}

// ---- Findings by asset (자산 보기) ----
export type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode는 자산 트리의 노드 하나다. key 형식: a:<id>(자산), c:<id>(기업),
// r:<domain>(DB에 자산 행이 없는 루트 도메인), __none__(연결된 자산 없음).
export interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // 이 자산에 바로 연결된 발견 사항 수
  total: number; // 하위 노드 포함, 발견 사항 기준으로 중복 제거
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// FINDING_UNASSIGNED_ASSET은 백엔드 db.FindingUnassignedAsset에 대응한다.
export const FINDING_UNASSIGNED_ASSET = "__none__";

// ---- Activity / sessions ----
export type ActivityKind =
  | "tool_use"
  | "tool_result"
  | "text"
  | "thinking"
  | "result"
  | "user"
  | "intent" // LLM-generated exploration objective leading a worker session (UI-synthesized)
  | "round" // planner round boundary marker (engine-emitted)
  | "usage" // live cumulative token usage (per model turn); not rendered
  | "llm_switch" // automatic/manual task-level LLM switch
  | "llm_failover" // task-level provider switch / chain exhaustion audit event
  | "intercept_request"; // user-approval request from the intercept layer

// ChatAttachment는 한 번 업로드한 파일이다: path는 해당 세션·작업의 작업 디렉터리(곧 에이전트의 CWD) 기준 상대 경로다.
export interface ChatAttachment {
  name: string;
  path: string;
  size: number;
  abs?: string; // 절대 경로(scope=staging 임시 업로드 때 돌려준다. 작업을 만들기 전에 설명에 넣는다)
}

export interface Activity {
  seq: number;
  intent_id?: string;
  worker: string; // session owner: planner | mainagent | work#1 ...
  ts: string;
  kind: ActivityKind;
  tool?: string;
  tool_use_id?: string;
  is_error?: boolean;
  summary: string;
  detail?: string;
  metadata?: {
    llm_transition?: LLMTransition;
  };
  source_task_id?: string;
  inherited?: boolean;
  main_seg?: number; // main-agent conversation segment (present only on worker="mainagent" rows)
  // token usage (present only on kind='result')
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

export interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- Agent triggers (P3 스케줄링, 사용자 지정 에이전트 전용) ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 정기 실행: N초마다(0이면 정기 실행 안 함)
  on_finding: boolean; // 어느 작업에서든 finding을 찾으면 실행
  on_goal_met: boolean; // 어느 작업에서든 목표를 달성하면 실행
  on_task_timeout: boolean; // 어느 작업에서든 시간 초과가 나면 실행
  on_tool_call: boolean; // 고른 도구가 호출되면(실행 완료) 실행
  on_task_create: boolean; // 어느 작업이든 만들어지면 실행
  interval_message: string; // 트리거 조건마다 따로 쓰는 사용자 메시지
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call에서 고른 도구 key(하나 이상)
  last_fire?: string;
}

// ---- Conversations (chat page) ----
export interface ActiveFindingRetest {
  id: number;
  finding_id: string;
  conversation_id: number;
  status: "pending" | "running";
}

export interface FindingRetest {
  id: number;
  finding_id: number;
  conversation_id: number | null;
  status: "pending" | "running" | "completed" | "failed" | "stopped";
  verdict: "" | "reproduced" | "fixed" | "inconclusive";
  notes: string;
  summary: string;
  evidence: string;
  error: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Conversation {
  id: number;
  running?: boolean; // live server state, returned with the conversation list
  agent_key: string;
  title: string;
  llm_profile_id?: number;
  pinned?: boolean;
  pinned_at?: string | null;
  created_at: string;
  updated_at: string;
}

// ---- Backend logs (/logs page) ----
export interface LogLine {
  seq: number;
  db_id?: number; // server_logs.id; present for DB-persisted lines
  ts: string;
  level: "info" | "warn" | "error";
  tag: string;
  text: string;
}

export type SessionRole = "mainagent" | "planner" | "worker" | "system";
export type SessionStatus = "running" | "paused" | "done" | "blocked" | "exhausted" | "pending" | "stopped" | "deleted";

// Daily token aggregate bucket (GET /api/tokens/daily).
export interface DailyTokenBucket {
  date: string; // "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Per-worker token usage (GET /api/exploration/tokens).
export interface TokenUsage {
  worker: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface SessionTokenUsage {
  session: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface BatchControlItem {
  id: string;
  ok: boolean;
  status?: string;
  queued?: boolean;
  error?: string;
}

// 분류 일괄 변경의 작업별 결과. 분류 쓰기 자체는 원자적이라 실패는 작업이 이미 삭제된 경우뿐이다.
export interface BatchCategoryItem {
  id: string;
  ok: boolean;
  error?: string;
}

// Whole-task (all agents) token aggregate.
export interface TokenTotal {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Global per-profile token spend from the llm_usage ledger (GET /api/tokens/usage).
export interface ProfileUsage {
  profile_name: string;
  calls: number;
  tasks: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// One (profile, UTC day) token bucket for the dashboard's daily chart (new source).
export interface ProfileDayUsage {
  profile_name: string;
  date: string; // YYYY-MM-DD
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

// Response of GET /api/tokens/usage — the dashboard's "new" (llm_usage) token view.
export interface UsageStats {
  by_profile: ProfileUsage[];
  daily: ProfileDayUsage[];
}

// Per-model token usage for one task (GET /api/llm/records/by-model), from the
// always-on llm_usage metering ledger. calls = number of LLM calls on this model.
export interface ModelTokenStat {
  model: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface Session {
  id: string;
  role: SessionRole;
  title: string;
  status: SessionStatus;
  live: boolean;
  last_activity: string;
  intent_id?: string;
  source_task_id?: string;
  inherited?: boolean;
  seg?: number; // main-agent session: which conversation segment (0 = original)
}

// ---- Security ----
export interface AuditEntry {
  ts: string;
  tool: string;
  action: "allow" | "block";
  reason?: string;
  command?: string;
}

export interface Audit {
  entries?: AuditEntry[];
  attributions?: Record<string, number>;
}

// ---- Traffic ----
export interface TrafficExchange {
  id: string;
  ts: string;
  host: string;
  method: string;
  url: string;
  status: number;
  content_type: string;
  resp_len: number;
}

export interface TrafficResp {
  enabled: boolean;
  proxy?: string;
  count?: number; // global total (unfiltered)
  total?: number; // rows matching the current filter (for pagination)
  page?: number;
  size?: number;
  exchanges?: TrafficExchange[];
}

// Full raw request/response of one exchange (lazy-loaded on row select).
export interface TrafficDetail {
  req: string;
  resp: string;
}

// One distinct recorded host with its exchange count (target picker).
export interface TrafficHost {
  host: string;
  count: number;
}

// ---- App settings (runtime toggles) ----
export interface Settings {
  traffic_capture: boolean;
  agent_traffic_binding: boolean; // 에이전트가 트래픽 증거를 자동으로 연결한다. 기본값 꺼짐. 수동 연결에는 영향이 없다
  llm_record: boolean; // LLM 기록 스위치(기본값 꺼짐). 끄면 LLM 호출을 하나도 기록하지 않는다
  // Web search. brave_key_set / tavily_key_set reflect whether a key is stored
  // (the values are never returned). On PUT, send the corresponding field to set/clear.
  web_search_enabled: boolean;
  web_search_backend: string; // "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // write-only: only sent on PUT to store/clear the key.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 검색 엔드포인트에 접근할 때 쓰는 별도 아웃바운드 프록시(http/https/socks5). 트래픽을 기록하는 MITM 프록시와 관계없다. 비면 직접 연결.
  web_search_proxy?: string;
  // 전역 아웃바운드 프록시(http/https/socks5, user:pass 가능). 모든 대상 트래픽이 이를 지난다. 트래픽 캡처를 켜면
  // MITM 업스트림이 되고, 끄면 에이전트의 bash/WebFetch에 바로 넣는다. 비면 직접 연결.
  global_proxy?: string;
  python_interpreter?: string; // 사용자 지정 스크립트 도구의 python 인터프리터 경로(비면 실행 시 탐지)
  workers?: number; // 동시에 도는 워커 에이전트 수(기본값 3). 이후 시작하는 작업부터 적용
  // 작업 동시 실행 제한: 동시에 '실행 중'인 작업 수의 최대값. 끄면 제한 없음. 켜면 최대를 넘는 새 작업은 대기하고 자리가 나면 자동으로 시작한다.
  task_concurrency_enabled?: boolean; // 기본값 false
  task_concurrency_limit?: number; // 켜면 기본값 5
  // LLM 장애 조치. 기본값 꺼짐. 켜면 '모델을 지정하지 않은' 에이전트는 현재 프로필을 쓸 수 없을 때
  // (잔액 부족, 키 무효, 속도 제한, 서비스 이상) 다음 프로필로 자동 전환한다.
  llm_pool_enabled?: boolean; // 기본값 false
  // 특정 프로필에 연결된 에이전트·작업이 실패할 때도 장애 조치 체인으로 넘길지 정한다. 기본값 false는 연결된 프로필만 쓴다는 뜻이다.
  llm_pool_bind_fallback?: boolean;
  // 작업 제약 조건을 넣을 범위(기본값 모두 켜짐): 작업의 allow/deny 제약 조건을 해당 에이전트의 시스템 프롬프트에 이어 붙인다.
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
  // 실험 기능: noa 모델 기반 컨텍스트 압축(기본값 꺼짐). 켜면 플랫폼에 연결된 네 종류의 에이전트(planner/
  // worker/메인 에이전트/대화)의 컨텍스트 압축을 내장 compaction 대신 noa가 맡는다. run마다 한 번 읽으며
  // 이후 시작하는 run부터 적용된다.
  noa_compaction?: boolean;
  // ---- 취약점 IM 알림 발송(알림 채널 자체는 별도 리소스로 /api/notify/*에 있고, 여기에는 전역 설정 세 가지만 있다) ----
  notify_enabled?: boolean; // 알림 발송 전체 스위치. 기본값 켜짐. 점검 중에 한 번에 멈추는 데 쓴다
  notify_public_base_url?: string; // 취약점 상세 링크에 쓰는 외부 접근 주소. 비면 메시지에 링크를 넣지 않는다
  notify_digest_interval_min?: number; // 다이제스트 모드 주기(분). 기본값 30
}

// ---- 취약점 IM 알림 발송 ----

// NotificationFilter는 알림 채널의 필터 조건이다. 필드는 모두 선택이며 없으면 거르지 않는다.
// 백엔드는 어떤 필드도 검사하지 않는다: 설정이 잘못됐으면 '일치'로 처리한다(빠뜨리느니 더 보내는 편을 택한다).
export interface NotificationFilter {
  min_severity?: string; // "" | low | medium | high | critical
  task_ids?: number[]; // 비면 제한 없음. 값이 있으면 취약점이 속한 작업과 겹쳐야 한다
  asset_ids?: number[]; // 비면 제한 없음. 값이 있으면 취약점에 연결된 자산과 겹쳐야 한다
  vulnclass_include?: string[]; // 비면 모두 받는다. 값이 있으면 취약점 유형이 키워드 중 하나와 일치해야 한다(대소문자 무시 부분 문자열)
  vulnclass_exclude?: string[]; // 키워드 중 하나라도 일치하면 제외한다(제외가 포함보다 우선)
  on_status_change?: boolean; // 취약점 처리 상태 변경 이벤트도 받을지 여부
}

// NotificationChannel은 알림 채널 인스턴스 하나다. config의 필드는 kind에 따라 다르고,
// 자격 증명 필드는 읽을 때 "__masked__"로 시작하는 가린 값으로 바뀐다. 그대로 돌려보내면 '바꾸지 않음'이라는 뜻이다.
export interface NotificationChannel {
  id: number;
  name: string;
  kind: string;
  enabled: boolean;
  mode: "realtime" | "digest";
  config: Record<string, unknown>;
  filter: NotificationFilter;
  rate_per_min: number;
  created_at: string;
  updated_at: string;
  // secret_keys는 백엔드가 알림 채널 유형별로 준다. 프런트는 이를 보고 비밀번호 칸과 '비워 두면 바꾸지 않음' 안내를 그리며,
  // 채널에 대한 지식을 하드코딩하지 않는다.
  secret_keys: string[];
}

// NotificationKind는 /api/notify/meta가 돌려주는 알림 채널 유형 메타데이터다.
export interface NotificationKind {
  kind: string;
  default_rate_per_min: number;
  secret_keys: string[];
}

export interface NotificationMeta {
  kinds: NotificationKind[];
  enabled: boolean;
  public_base_url: string;
  digest_interval_min: string;
  defaults: { digest_interval_min: number };
  stats: {
    channels: number;
    channels_on: number;
    pending: number;
    failed: number;
    sent_today: number;
    backlog_age_ms: number;
  };
}

// NotificationDelivery는 전달 기록 한 건이다. 전달 이력과 실패 재발송에 쓴다.
export interface NotificationDelivery {
  id: number;
  finding_id: string;
  event_kind: string; // finding_created | finding_status_changed
  channel_id: number;
  channel_name: string;
  channel_kind: string;
  state: "pending" | "sending" | "sent" | "failed" | "skipped";
  attempts: number;
  last_error: string;
  batch_id?: number;
  created_at: string;
  sent_at?: string;
  next_attempt_at: string;
  title: string;
  severity: string;
}

// ---- LLM config ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  proxy?: string;
  model: string;
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 사고 스위치(thinking.type): ""=보내지 않음(기본값) | "disabled"=꺼짐 | "enabled"=켜짐
  thinking_type?: string;
  // 사고 강도: ""=보내지 않음(기본값) | "low"/"medium"/"high"/"xhigh"/"max"
  reasoning_effort?: string;
  is_default: boolean;
  // 장애 조치 순서: 클수록 먼저 고른다. 활성 프로필은 이 값과 관계없이 항상 체인 맨 앞이다.
  priority?: number;
  // true면 장애 조치 대상에서 뺀다(에이전트·작업에 명시적으로 연결하면 여전히 쓸 수 있다).
  pool_exclude?: boolean;
  // true(기본값)=스트리밍(SSE) | false=완전한 비스트리밍(stream:false, 한 번에 돌려받음).
  streaming?: boolean;
  // 응답 한 번의 출력 최대 토큰. 0이면 이 필드를 보내지 않고 서버 기본값을 따른다.
  // context_window_k와 구분한다: 그쪽은 모델 전체 용량이며 로컬에서 압축 기준값으로만 쓴다.
  max_tokens?: number;
  // 최대값을 담을 요청 필드 이름. format="openai"일 때만 의미가 있다:
  // ""=max_tokens(기본값) | "max_completion_tokens"(OpenAI 추론 모델은 이것만 받는다)
  max_tokens_field?: string;
  // 사용자 지정 세션 헤더 이름: 값이 있으면 요청마다 이 HTTP 헤더를 붙이고, 헤더 값은 현재 세션·탐색 의도의 session id다.
  // ""=보내지 않음. session-id 헤더로 프롬프트 캐시나 고정 라우팅을 하는 게이트웨이에 쓴다.
  session_header_key?: string;
  // 이 프로필의 재시도 덮어쓰기(연결 / 빈 응답 / 같은 제공자 안전 구간). 비우거나 모두 0이면 전역 정책을 따른다.
  retry?: LLMRetryOverride;
  // 인증 방식. 옛 응답에는 없을 수 있어 비면 api_key로 본다.
  auth_type?: LLMAuthType;
  // 구독 프로필의 연결 상태. 토큰은 서버가 싣지 않는다.
  oauth?: LLMProfileOAuth;
}

export type LLMAuthType = "api_key" | "chatgpt_oauth" | "claude_oauth";

export interface LLMProfileOAuth {
  connected: boolean;
  expires_at?: string;
  // 마지막 토큰 갱신이 재로그인을 요구하며 거절됐다(서버 메모리 상태라 재시작 뒤 false).
  needs_login: boolean;
  plan?: string;
}

// ChatGPT 구독 로그인 API 응답. flow_id 외에 토큰·code·state는 오지 않는다.
export interface ChatGPTLoginStart {
  flow_id: string;
  authorize_url: string;
  expires_at: string;
}

export interface ChatGPTDeviceStart {
  flow_id: string;
  user_code: string;
  verification_url: string;
  expires_at: string;
}

export type ChatGPTDeviceStatus = "pending" | "succeeded" | "failed" | "expired";

export interface ChatGPTDevicePoll {
  status: ChatGPTDeviceStatus;
  code?: string;
}

// ---- LLM 재시도 정책 ----
// 재시도 한 계층의 설정 두 가지. 둘 다 '0 = 설정 안 됨'이다:
//   attempts    0=기본 횟수 | -1=이 계층의 재시도 끄기 | >0=재시도 횟수
//   interval_ms 0=기본 지수 백오프 | >0=이 고정 밀리초 간격을 쓴다
export interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// LLM 프로필 하나가 덮어쓸 수 있는 세 계층(모두 '엔드포인트를 따라가는' 재시도).
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 연결 재시도: 연결 재설정 / 시간 초과 / 429 / 5xx, 스트림 시작 전
  empty: LLMRetryRule; // 빈 응답 재시도: 완료됐지만 내용이 하나도 없을 때(openai 형식만)
  stream: LLMRetryRule; // 같은 제공자 안전 구간 재시도: 출력을 넘기기 전에 스트림이 끊기면 다시 보낸다
}

// 전역 정책 = 위 세 계층의 기본값 + 전역에만 있는 두 계층:
//   breaker 장애 조치 회로 차단기(attempts=일시적 실패가 몇 번 이어지면 차단할지, interval_ms=고정 대기 시간)
//   intent  탐색 의도 재실행(워커가 model_error로 끝나면 탐색 의도 전체를 재실행)
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM 장애 조치 ----
// 프로필 하나의 장애 조치 체인 안 위치와 상태. state:
//   ok       정상
//   degraded 실패가 이어졌지만 차단 기준에 이르지 않음
//   tripped  차단됨. 대기 시간 동안 건너뛴다(cooldown_secs는 남은 초)
export interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // 현재 활성 프로필인지(항상 체인 맨 앞)
  excluded: boolean; // pool_exclude: 장애 조치에 참여하지 않음
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
}

// ---- Agents ----
export interface Agent {
  id: string;
  key: string; // 내장은 goals/planner/mainagent/worker, 사용자 지정은 사용자가 정한 key
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // 연결된 LLM 프로필. null이거나 없으면 작업·세션·전역 설정을 따른다
  max_turns?: number; // 0 = 제한 없음
  run_seconds?: number; // 워커 한 번 실행의 실제 경과 시간 최대값(초). 0 = 제한 없음
  web_search?: boolean; // 웹 검색 사용 여부(시스템 전역 스위치가 켜져 있어야 한다)
  interactive_shell?: boolean; // 대화형 shell 사용 여부(지속 PTY 세션 도구 묶음)
  // P3 트리거 뒤 처리 정책(사용자 지정 에이전트에서만 의미가 있다)
  trigger_run_mode?: "serial" | "parallel"; // 순차 대기 / 트리거마다 세션 하나씩 동시 실행
  trigger_merge_mode?: "by_task" | "all" | "none"; // serial에서만: 같은 작업끼리 합침 / 모두 합침 / 합치지 않음
  trigger_max_parallel?: number; // parallel에서만: 에이전트별 동시 실행 최대값. 0=제한 없음
  // 연결 개수(목록 API만 돌려준다): 공개 MCP / 공개 스킬 / 연결된 도구
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // 연결할 수 있는 LLM 프로필 후보('기본 모델' 드롭다운용). 현재 연결은 agent.llm_profile_id에 있다
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 저장된 마무리 프롬프트(비면 내장 기본값을 쓴다)
  wrapup_default?: string; // 내장 기본 마무리 프롬프트(자리표시자 / 기본값 복원용)
  wrapup_max_turns?: number; // 저장된 마무리 턴 수(0이면 내장 기본값을 쓴다)
  wrapup_max_turns_default?: number; // 내장 기본 마무리 턴 수("0=기본값 N" 안내용)
  // 작업 단위 시간 초과 마무리 프롬프트(worker/planner만. task_timeout_wrapup_supported=true일 때만 이 영역을 보인다)
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}

// ---- MCP ----
export interface MCPServer {
  id: number;
  name: string;
  transport: "stdio" | "http" | "sse";
  command?: string;
  args: string[];
  env: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure?: boolean; // http: skip TLS cert verification (self-signed servers)
  tools?: string[]; // mcp_tools_cache (names only, for the count)
}

export interface MCPTool {
  name: string;
  description: string;
}

// ---- Skills ----
// Fields align with the agentskills.io open specification.
// description covers both "what the skill does" and "when to use it".
export interface SkillItem {
  name: string; // unique key = directory name
  description?: string; // required per spec; covers what + when to use
  license?: string; // optional: SPDX identifier or free text
  compatibility?: string; // optional: environment requirements
  mcps?: string[]; // MCP server names this skill unlocks on load
  files: string[]; // files in the skill directory
  // 호출 통계(skill_usage 원장). 한 번도 호출되지 않은 스킬은 calls=0이고 last_used가 없다.
  calls: number;
  tasks: number; // 이 스킬을 불러온 작업 수(chat 세션은 세지 않는다)
  usage_agents: string[]; // 이 스킬을 불러온 에이전트 key
  last_used?: string;
}

// SkillCall은 Skill() 호출 한 번이다(스킬 하나의 최근 호출 목록).
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0 = 작업이 아닌 경우(대화 세션)
  session_id: string;
  args_len: number;
}

// MissingSkill은 이름으로 불렀지만 없는 스킬이다. '쓰려 했지만 없는' 빈자리를 보여 준다.
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}

// ---- Tools (내장 도구 목록) ----
// key + handler live in Go; only these fields are page-editable. system tools lock
// the key and the parameter *structure* (name/type/required) — the per-param
// description/default and the agent binding are what move.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  schema: Record<string, any>; // full JSON-Schema (object with properties)
  agents: string[]; // bound agent keys
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // 사용자 지정 도구 유형
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  exec?: Record<string, any>; // 사용자 지정 도구 실행 명세(kind!=builtin)
  deferred?: boolean; // schema 지연 로드(SearchExtraTools/ExecuteExtraTool)
  calls?: number; // persistent runtime invocation count (older APIs may omit it)
}

// ---- Stats ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}

// ---- Intercept Rules ----
export type InterceptAction = "allow" | "deny" | "ask";
export type InterceptMatchTarget = "tool_name" | "tool_input";
export type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

// ---- Asset Intercept Rules(자산 차단: 전역 차단 목록) ----
export type AssetInterceptKind =
  | "exact_domain"
  | "exact_ip"
  | "exact_url"
  | "fuzzy_domain"
  | "fuzzy_ip"
  | "fuzzy_url"
  | "cidr";

// action은 작업 단위 규칙에만 쓴다: block=차단(테스트 금지) allow=허용(허용 목록).
export type AssetInterceptAction = "block" | "allow";

// 작업 단위 자산 차단·허용 규칙의 입력 항목(작업 만들기, 작업 상세 편집에서 쓴다).
export interface AssetInterceptRuleInput {
  action: AssetInterceptAction;
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  enabled: boolean;
}

export interface AssetInterceptRule {
  id: number;
  enabled: boolean;
  action?: AssetInterceptAction; // 전역 규칙에는 이 필드가 없다(항상 차단). 작업 단위 규칙은 block/allow를 나눈다
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // 규칙 message 또는 모델 판정 이유(모델 판정에는 [모델] 접두사가 붙는다. #110 이전 행은 [模型])
  decided_at?: string;
  created_at: string;
}

// JudgeConfig: 모델 승인 심사(차단 규칙에 하나도 일치하지 않을 때만 모델이 판정)의 전역 설정.
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0 = 활성·기본 프로필을 따른다
  prompt: string; // 판정 프롬프트. 설정하지 않았으면 GET 때 백엔드가 내장 템플릿 전문을 채운다
  timeout_seconds: number; // 모델 호출 시간 제한
  fail_action: "allow" | "ask" | "deny"; // 모델 오류 / 시간 초과 / 파싱할 수 없을 때의 대체 동작
  ask_timeout_seconds: number; // 모델이 ask로 판정해 승인 요청으로 넘긴 뒤 승인 심사를 기다리는 시간 제한
  ask_timeout_action: "allow" | "deny"; // 승인 심사 시간 초과 뒤 기본 동작
}

// JudgeUsage: 모델 승인 심사(judge 경로)의 누적 토큰 사용량 + 최근 N일 일별 값.
export interface JudgeDayUsage {
  date: string; // YYYY-MM-DD (UTC)
  calls: number;
  input_tokens: number;
  output_tokens: number;
}
export interface JudgeUsage {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  daily: JudgeDayUsage[];
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "unknown";
}

// InterceptApprovalRow enriches InterceptPending with conversation/task and rule context.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // "" if no linked conversation
  conv_agent_key: string; // "" if no linked conversation
  rule_name: string; // "" if rule was deleted
}

// ── 자산 동기화 (ScopeSentry 데이터 소스) ──────────────────────────────────────────────
export interface SSProject {
  id: string; // MongoDB ObjectID — used as filter.project
  name: string;
  logo?: string;
  AssetCount?: number;
  tag?: string;
}

export interface SSTask {
  id: string;
  name: string; // used as filter.task
  status?: number;
  progress?: number;
  creatTime?: string;
  endTime?: string;
}

// ConvTokenSummary — one conversation's token total (+ profile/date) for merging
// chat usage into the dashboard token stats. GET /api/tokens/conversations.
export interface ConvTokenSummary {
  llm_profile_id: number | null;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- Command recording (Bash execution history) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // raw tool input (JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// 도구 하나의 호출 통계(/commands/stats). errors는 그중 실패한 횟수다.
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}

// ---- LLM recording ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // 제공자와 실제로 주고받은 HTTP 원본: 요청은 buildBody()가 보낸 전체 body(도구
  // schema 포함), 응답은 원본 SSE 프레임이다. 위의 request_body/response_body는 정규화한 보기로
  // 도구 schema와 tool_use 블록을 버린다. 옛 기록에서는 비어 있다.
  raw_request?: string;
  raw_response?: string;
}

// One distinct task with its LLM-record count (task picker on the records page).
export interface LLMTask {
  task_id: string;
  count: number;
}

// The exact JSON sent to the review model, retained for all model verdicts.
export interface InterceptReviewInput {
  version: number;
  background?: {
    // worker_summary is retained only for immutable v2/v3 snapshots.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // Version 1 snapshots are immutable and remain readable in historical audits.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // Legacy v1/v2 snapshots only; v3 never sends execution history.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// Immutable review snapshot plus separately recorded execution outcome.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}
export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

export type TrafficEvidenceRole = "baseline" | "proof" | "verification" | "supporting";
export interface TrafficEvidenceRef {
  traffic_id: string;
  role?: TrafficEvidenceRole;
  note?: string;
}
export interface TrafficEvidenceSnapshot {
  id: string;
  source_traffic_id: string;
  captured_at: number;
  url: string;
  method: string;
  status: number;
  content_type: string;
  req_head?: string;
  resp_head?: string;
  req_hash: string;
  resp_hash: string;
  req_len: number;
  resp_len: number;
}
export interface FindingTrafficBinding {
  id: string;
  finding_id: string;
  snapshot_id: string;
  role: TrafficEvidenceRole;
  note: string;
  position: number;
  created_at: string;
  snapshot: TrafficEvidenceSnapshot;
}
export interface FindingTraffic {
  finding_id: string;
  version: number;
  report_version: number;
  bindings: FindingTrafficBinding[];
}
export interface EvidenceBodyPreview {
  content: string;
  offset: number;
  total: number;
  next_offset: number;
  truncated: boolean;
  binary: boolean;
}
export interface FindingTrafficDetail {
  binding: FindingTrafficBinding;
  request: EvidenceBodyPreview;
  response: EvidenceBodyPreview;
}

/** GET /api/update/check: 현재 버전과 GitHub 최신 정식 버전을 비교한 결과. */
export interface UpdateCheck {
  /** 지금 실행 중인 버전. 개발 빌드는 "dev"이거나 git describe의 접미사 붙은 형태다. */
  current: string;
  /** 실행 형태. docker에서는 교체가 컨테이너 쓰기 계층에만 적용되어, 컨테이너를 다시 만들면 이미지 버전으로 돌아간다. */
  mode: "docker" | "binary";
  os: string;
  arch: string;
  repo: string;
  /** 롤백할 수 있는 이전 버전(artex.old)이 있는지. */
  has_backup: boolean;
  /** 이번 시작 때 자체 업데이트 부트스트랩의 결과(교체 실패 / 롤백됨 등). 아무 일도 없으면 비어 있다. */
  boot_notice?: string;
  rolled_back?: boolean;
  /** GitHub 조회에 실패하면 이유를 준다. 이때 아래 필드는 모두 없다. */
  error?: string;
  latest?: string;
  notes?: string;
  html_url?: string;
  published_at?: string;
  /** 현재 플랫폼에 맞는 배포 파일 이름과, 그 Release에 실제로 그 파일이 있는지. */
  asset?: string;
  asset_available?: boolean;
  size?: number;
  has_update?: boolean;
  /** 두 버전 번호를 비교할 수 있는지. 개발 빌드는 false이며 이때 원클릭 업데이트를 막는다. */
  comparable?: boolean;
  /** comparable이 false일 때의 설명. */
  reason?: string;
}

/** /api/update/stream이 보내는 업데이트 진행 상황 한 건. */
export interface UpdateProgress {
  phase: "idle" | "downloading" | "verifying" | "extracting" | "staged" | "failed";
  /** 다운로드 단계에서만 의미가 있다(0-100). 나머지 단계는 -1. */
  percent: number;
  message: string;
  version?: string;
  error?: string;
}

// Original execution selected from an approval, never submitted to the reviewer.
export interface InterceptExecution {
  conversation_id: number | null;
  task_id: string | null;
  session: string;
  seq: number;
  items: Activity[];
}
