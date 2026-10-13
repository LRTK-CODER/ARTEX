// LLM 프로필 폼의 모델 선택 목록을 만드는 순수 로직.
// 정적 추천 목록은 조회가 안 될 때(로그인 전, 조회 실패)의 폴백이다. 실제 조회 결과가 언제나 앞선다.
// 최신으로 유지하는 것이 목적이 아니므로, 백엔드 기본값(agent/provider.go)과 알려진 구독 모델만 둔다.
import type { LLMAuthType, LLMProfileOAuth } from "./types";

export type LLMFormat = "anthropic" | "openai" | "openai-responses";

const CLAUDE_MODELS = ["claude-opus-4-8", "claude-opus-4-6", "claude-sonnet-4-5", "claude-haiku-4-5"] as const;
const CHATGPT_SUBSCRIPTION_MODELS = ["gpt-5", "gpt-5-codex", "gpt-5.5"] as const;
const OPENAI_MODELS = ["gpt-5", "gpt-4o"] as const;

// 인증 방식과 형식에 맞는 정적 추천 모델. 구독은 형식이 인증 방식으로 정해지므로 인증 방식만 본다.
export function suggestedModels(authType: LLMAuthType, format: LLMFormat): readonly string[] {
  if (authType === "claude_oauth") return CLAUDE_MODELS;
  if (authType === "chatgpt_oauth") return CHATGPT_SUBSCRIPTION_MODELS;
  return format === "anthropic" ? CLAUDE_MODELS : OPENAI_MODELS;
}

// 조회한 목록을 앞에, 그 뒤에 정적 목록 중 겹치지 않는 것을 둔다.
// 저장된 모델이 어느 목록에도 없으면 맨 앞에 넣는다. 편집할 때 목록에 없는 값도 그대로 보이고 고를 수 있어야 한다.
export function mergeModelOptions(fetched: readonly string[], suggested: readonly string[], current: string): string[] {
  const options: string[] = [];
  const seen = new Set<string>();
  for (const raw of [...fetched, ...suggested]) {
    const model = raw.trim();
    if (!model || seen.has(model)) continue;
    seen.add(model);
    options.push(model);
  }
  const saved = current.trim();
  if (saved && !seen.has(saved)) options.unshift(saved);
  return options;
}

// 모델 조회 요청에 들어가는 값. 주소·프록시·키는 사용자가 칸에 치는 값이다.
export interface ModelFetchConnection {
  baseUrl: string;
  proxy: string;
  apiKey: string;
}

export interface ModelFetchRequest extends ModelFetchConnection {
  format: LLMFormat;
  authType: LLMAuthType;
  profileId?: number;
}

// 요청을 가르는 키. 응답이 지금 화면의 형식·인증·주소와 같은 요청에서 왔는지 비교하는 데 쓴다.
export function modelFetchKey(request: ModelFetchRequest): string {
  return JSON.stringify([
    request.format,
    request.authType,
    request.profileId ?? null,
    request.baseUrl.trim(),
    request.proxy.trim(),
    request.apiKey.trim(),
  ]);
}

// 주소·프록시·키 칸의 상태. live는 지금 칸에 든 값, committed는 칸을 벗어날 때(blur) 확정한 값이다.
// 자동 조회는 committed만 쓴다. 치는 동안의 중간 값(예: https://api.openai.co)으로 저장된 키가 나가지 않게 한다.
export interface ModelFetchFields {
  live: ModelFetchConnection;
  committed: ModelFetchConnection;
}

export type ModelFetchSelection = Pick<ModelFetchRequest, "format" | "authType" | "profileId">;

// 패널을 열 때. 저장된 값은 이미 확정된 값이다.
export function openFields(saved: ModelFetchConnection): ModelFetchFields {
  return { live: saved, committed: saved };
}

// 칸에 칠 때. live만 바뀌고 committed는 같은 객체로 남는다.
export function typeFields(fields: ModelFetchFields, patch: Partial<ModelFetchConnection>): ModelFetchFields {
  return { live: { ...fields.live, ...patch }, committed: fields.committed };
}

// 칸을 벗어날 때 live를 확정한다. 값이 같으면(앞뒤 공백 무시) committed를 같은 객체로 둔다.
export function blurFields(fields: ModelFetchFields): ModelFetchFields {
  const { live, committed } = fields;
  const isSame =
    committed.baseUrl.trim() === live.baseUrl.trim() &&
    committed.proxy.trim() === live.proxy.trim() &&
    committed.apiKey.trim() === live.apiKey.trim();
  return isSame ? fields : { live, committed: { ...live } };
}

// 자동 조회에 보낼 요청. 형식·인증 방식은 고르는 즉시, 주소·프록시·키는 확정된 값만 쓴다.
// 화면 상태 전체를 받아 여기서 고르므로, 칸에 치는 값이 자동 조회에 섞이지 않는 것을 테스트로 지킬 수 있다.
export function autoFetchRequest(selection: ModelFetchSelection, fields: ModelFetchFields): ModelFetchRequest {
  return { ...selection, ...fields.committed };
}

// 다시 불러오기 버튼에 보낼 요청. 사용자가 누른 것이므로 지금 칸에 든 값을 그대로 쓴다.
export function manualFetchRequest(selection: ModelFetchSelection, fields: ModelFetchFields): ModelFetchRequest {
  return { ...selection, ...fields.live };
}

// 구독 프로필이 지금 인증 방식으로 저장돼 로그인된 상태인가. oauth는 패널 안 로그인·해제로 바뀐 최신 상태다.
export function isSubscriptionConnected(
  savedAuthType: LLMAuthType | undefined,
  authType: LLMAuthType,
  oauth: LLMProfileOAuth | undefined,
): boolean {
  return savedAuthType === authType && Boolean(oauth?.connected) && !oauth?.needs_login;
}

export interface ModelFetchTicket {
  seq: number;
  key: string;
}

// 응답을 목록에 반영해도 되는가. 뒤에 다른 요청이 나갔거나(번호가 다름), 요청 뒤 형식·인증·주소가
// 바뀌었으면(키가 다름) 늦게 온 응답이라 버린다.
export function isFreshModelFetch(ticket: ModelFetchTicket, latestSeq: number, currentKey: string): boolean {
  return ticket.seq === latestSeq && ticket.key === currentKey;
}

export type ModelFetchOrigin = "auto" | "manual";

// 응답이 올 때 비교할 지금 화면의 키. 자동 조회는 확정값으로, 버튼 조회는 입력값으로 만든다.
// 자동 조회 응답을 입력값과 비교하면 응답을 기다리는 동안 다른 칸을 치기만 해도 결과가 버려진다.
export function currentFetchKey(
  origin: ModelFetchOrigin,
  selection: ModelFetchSelection,
  fields: ModelFetchFields,
): string {
  const request = origin === "auto" ? autoFetchRequest(selection, fields) : manualFetchRequest(selection, fields);
  return modelFetchKey(request);
}
