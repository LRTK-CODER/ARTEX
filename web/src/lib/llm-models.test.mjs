import {
  autoFetchRequest,
  blurFields,
  currentFetchKey,
  isFreshModelFetch,
  isSubscriptionConnected,
  manualFetchRequest,
  mergeModelOptions,
  modelFetchKey,
  openFields,
  suggestedModels,
  typeFields,
} from "./llm-models.ts";
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

const savedConnection = { baseUrl: "https://api.openai.com/v1", proxy: "", apiKey: "" };
const selection = { format: "openai", authType: "api_key", profileId: 7 };
const autoKey = (fields) => modelFetchKey(autoFetchRequest(selection, fields));

test("Base URL·프록시·API 키를 치는 동안에는 자동 조회 요청이 바뀌지 않는다", () => {
  const opened = openFields(savedConnection);
  const typing = typeFields(
    typeFields(typeFields(opened, { baseUrl: "https://api.openai.co" }), { proxy: "http://127.0.0.1:80" }),
    { apiKey: "sk-ne" },
  );
  assert.deepEqual(autoFetchRequest(selection, typing), { ...selection, ...savedConnection });
  assert.equal(autoKey(typing), autoKey(opened));
  // 버튼 조회는 지금 칸의 값을 쓰므로 키가 달라야 한다. 같으면 위 단언이 입력값을 보지 않은 것이다.
  assert.notEqual(modelFetchKey(manualFetchRequest(selection, typing)), autoKey(opened));
});

test("칸을 벗어나면 입력값이 확정돼 자동 조회 요청이 바뀐다", () => {
  const typed = typeFields(openFields(savedConnection), { baseUrl: "https://gateway.example.com/v1" });
  const blurred = blurFields(typed);
  assert.equal(autoFetchRequest(selection, blurred).baseUrl, "https://gateway.example.com/v1");
  assert.notEqual(autoKey(blurred), autoKey(typed));
});

test("값을 바꾸지 않고 칸을 벗어나면 같은 상태를 돌려 다시 조회하지 않는다", () => {
  const opened = openFields(savedConnection);
  const typed = typeFields(opened, { baseUrl: `  ${savedConnection.baseUrl} ` });
  assert.equal(blurFields(typed), typed);
  assert.equal(blurFields(typed).committed, opened.committed);
});

test("형식이나 인증 방식을 바꾸면 자동 조회 키가 바로 바뀐다", () => {
  const fields = openFields(savedConnection);
  const before = autoKey(fields);
  assert.notEqual(modelFetchKey(autoFetchRequest({ ...selection, format: "anthropic" }, fields)), before);
  assert.notEqual(modelFetchKey(autoFetchRequest({ ...selection, authType: "chatgpt_oauth" }, fields)), before);
});

test("자동 조회 응답은 기다리는 동안 다른 칸을 쳐도 버려지지 않는다", () => {
  const opened = openFields(savedConnection);
  const ticket = { seq: 1, key: modelFetchKey(autoFetchRequest(selection, opened)) };
  const typing = typeFields(opened, { proxy: "socks5://127.0.0.1:1080" });
  assert.equal(isFreshModelFetch(ticket, 1, currentFetchKey("auto", selection, typing)), true);
  // 칸을 벗어나 확정하면 조건이 바뀐 것이므로 버린다.
  assert.equal(isFreshModelFetch(ticket, 1, currentFetchKey("auto", selection, blurFields(typing))), false);
});

test("버튼 조회 응답은 기다리는 동안 칸 값이 바뀌면 버린다", () => {
  const opened = openFields(savedConnection);
  const ticket = { seq: 1, key: modelFetchKey(manualFetchRequest(selection, opened)) };
  assert.equal(isFreshModelFetch(ticket, 1, currentFetchKey("manual", selection, opened)), true);
  const typing = typeFields(opened, { proxy: "socks5://127.0.0.1:1080" });
  assert.equal(isFreshModelFetch(ticket, 1, currentFetchKey("manual", selection, typing)), false);
});

test("구독 연결 상태는 최신 oauth 값으로 판단한다", () => {
  const cases = [
    {
      name: "로그인 직후",
      saved: "chatgpt_oauth",
      auth: "chatgpt_oauth",
      oauth: { connected: true, needs_login: false },
      want: true,
    },
    {
      name: "해제 뒤",
      saved: "chatgpt_oauth",
      auth: "chatgpt_oauth",
      oauth: { connected: false, needs_login: false },
      want: false,
    },
    {
      name: "다시 로그인 필요",
      saved: "claude_oauth",
      auth: "claude_oauth",
      oauth: { connected: true, needs_login: true },
      want: false,
    },
    {
      name: "방식을 바꾼 채 저장 전",
      saved: "api_key",
      auth: "claude_oauth",
      oauth: { connected: true, needs_login: false },
      want: false,
    },
    { name: "연결 정보 없음", saved: "chatgpt_oauth", auth: "chatgpt_oauth", oauth: undefined, want: false },
  ];
  for (const c of cases) assert.equal(isSubscriptionConnected(c.saved, c.auth, c.oauth), c.want, c.name);
});

test("늦게 온 조회 응답은 버린다", () => {
  const request = { ...selection, ...savedConnection };
  const ticket = { seq: 3, key: modelFetchKey(request) };
  assert.equal(isFreshModelFetch(ticket, 3, modelFetchKey(request)), true, "마지막 요청이고 조건이 같다");
  assert.equal(isFreshModelFetch(ticket, 4, modelFetchKey(request)), false, "뒤에 다른 요청이 나갔다");
  assert.equal(
    isFreshModelFetch(ticket, 3, modelFetchKey({ ...request, format: "anthropic" })),
    false,
    "요청 뒤 형식이 바뀌었다",
  );
  assert.equal(
    isFreshModelFetch(ticket, 3, modelFetchKey({ ...request, baseUrl: "https://other.example.com" })),
    false,
    "요청 뒤 주소가 바뀌었다",
  );
});
