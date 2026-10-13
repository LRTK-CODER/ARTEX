// LLM 프로필 폼의 모델 선택 목록을 만드는 순수 로직.
// 정적 추천 목록은 조회가 안 될 때(로그인 전, 조회 실패)의 폴백이다. 실제 조회 결과가 언제나 앞선다.
// 최신으로 유지하는 것이 목적이 아니므로, 백엔드 기본값(agent/provider.go)과 알려진 구독 모델만 둔다.
import type { LLMAuthType } from "./types";

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
