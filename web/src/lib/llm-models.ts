// LLM 프로필 폼의 모델 선택 목록을 만드는 순수 로직.
// 목록은 실제 조회 결과와 편집 중인 프로필에 저장된 값만 쓴다. 정적 추천 목록은 두지 않는다.
// 자격 증명이 없는 프로필은 어차피 호출할 수 없으므로, 그때는 목록 대신 로그인·키 안내를 보여 준다.
import type { LLMAuthType, LLMProfileOAuth } from "./types";

export type LLMFormat = "anthropic" | "openai" | "openai-responses";

// 조회한 목록에서 빈 값과 중복을 뺀다. 저장된 모델이 목록에 없으면 맨 앞에 넣는다.
// 편집할 때 목록에 없는 값(로그인 전에 저장한 값, 조회되지 않는 값)도 그대로 보이고 고를 수 있어야 한다.
export function mergeModelOptions(fetched: readonly string[], current: string): string[] {
  const options: string[] = [];
  const seen = new Set<string>();
  for (const raw of fetched) {
    const model = raw.trim();
    if (!model || seen.has(model)) continue;
    seen.add(model);
    options.push(model);
  }
  const saved = current.trim();
  if (saved && !seen.has(saved)) options.unshift(saved);
  return options;
}

export function isSubscriptionAuth(authType: LLMAuthType): boolean {
  return authType === "chatgpt_oauth" || authType === "claude_oauth";
}

// 저장할 수 없는 이유. 빈 문자열이면 저장할 수 있다.
// 구독 프로필은 저장한 뒤에야 로그인하고 모델을 조회할 수 있으므로 모델 없이 저장한다(빈 모델은 서버 기본값).
// API 키 프로필은 저장 전에 키로 조회할 수 있으므로 모델을 고르게 한다.
export function profileSaveError(name: string, model: string, authType: LLMAuthType): string {
  if (isSubscriptionAuth(authType)) return name.trim() ? "" : "이름을 입력하세요";
  if (!name.trim() || !model.trim()) return "이름과 모델을 입력하세요";
  return "";
}

// 화면에 보일 모델 이름. 빈 모델은 서버가 실제로 쓰는 기본 모델(default_model)을 함께 보인다.
export function modelLabel(model: string, defaultModel: string | undefined): string {
  const chosen = model.trim();
  if (chosen) return chosen;
  const fallback = defaultModel?.trim();
  return fallback ? `기본값(${fallback})` : "기본값";
}

export interface ModelsHintState {
  fetchedCount: number;
  error: string; // 조회 실패 사유. 비어 있으면 실패하지 않았다
  hasCredential: boolean;
  isSubscription: boolean;
}

// 모델 칸 아래 안내. 목록이 어디서 왔는지, 목록이 왜 비었는지, 무엇을 하면 목록이 나오는지 알린다.
export function modelsHint({ fetchedCount, error, hasCredential, isSubscription }: ModelsHintState): string {
  if (fetchedCount > 0) return `API에서 불러온 모델 ${fetchedCount}개를 보여 줍니다. 목록에 없으면 직접 입력하세요.`;
  if (!hasCredential) {
    if (isSubscription)
      return "저장한 뒤 로그인하면 쓸 수 있는 모델 목록이 나옵니다. 모델은 비워 둔 채 저장해도 되고, 비워 두면 기본 모델을 씁니다.";
    return "API 키를 입력하고 칸을 벗어나면 쓸 수 있는 모델 목록이 나옵니다. 직접 입력해도 됩니다.";
  }
  if (error) return `모델 목록을 불러오지 못했습니다(${error}). 다시 불러오거나 직접 입력하세요.`;
  return "모델 목록을 불러오는 중입니다. 직접 입력해도 됩니다.";
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
