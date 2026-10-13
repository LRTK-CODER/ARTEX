import { mergeModelOptions, suggestedModels } from "./llm-models.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("구독 형식은 인증 방식에 맞는 추천 모델을 준다", () => {
  // 형식 인자는 구독에서 무시된다. 구독의 형식은 인증 방식으로 정해지기 때문이다.
  assert.ok(suggestedModels("claude_oauth", "openai").includes("claude-opus-4-8"));
  assert.ok(suggestedModels("chatgpt_oauth", "anthropic").includes("gpt-5"));
});

test("API 키 방식은 형식에 맞는 추천 모델을 준다", () => {
  assert.ok(suggestedModels("api_key", "anthropic").includes("claude-opus-4-8"));
  assert.ok(suggestedModels("api_key", "openai").includes("gpt-4o"));
  assert.ok(suggestedModels("api_key", "openai-responses").includes("gpt-5"));
});

test("조회한 모델이 정적 목록보다 앞에 오고 중복은 한 번만 남는다", () => {
  const options = mergeModelOptions(["b", "a"], ["a", "c"], "");
  assert.deepEqual(options, ["b", "a", "c"]);
});

test("목록에 없는 저장된 모델은 맨 앞에 보존된다", () => {
  const options = mergeModelOptions(["a"], ["b"], "  custom-model  ");
  assert.deepEqual(options, ["custom-model", "a", "b"]);
});

test("목록에 이미 있는 저장된 모델은 자리를 옮기지 않는다", () => {
  const options = mergeModelOptions(["a", "b"], [], "b");
  assert.deepEqual(options, ["a", "b"]);
});

test("빈 문자열과 공백 항목은 버린다", () => {
  const options = mergeModelOptions([" ", "a "], [""], "");
  assert.deepEqual(options, ["a"]);
});
