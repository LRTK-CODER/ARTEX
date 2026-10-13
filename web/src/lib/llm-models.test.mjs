import * as models from "./llm-models.ts";
import {
  autoFetchRequest,
  blurFields,
  currentFetchKey,
  isFreshModelFetch,
  isSubscriptionAuth,
  isSubscriptionConnected,
  manualFetchRequest,
  mergeModelOptions,
  modelFetchKey,
  modelLabel,
  modelsHint,
  openFields,
  profileSaveError,
  typeFields,
} from "./llm-models.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("모델 목록은 조회 결과만으로 만들고 중복은 한 번만 남긴다", () => {
  assert.deepEqual(mergeModelOptions(["b", "a", "b"], ""), ["b", "a"]);
});

test("조회 결과가 없으면 목록이 비고, 정적 추천 모델을 채우지 않는다", () => {
  assert.deepEqual(mergeModelOptions([], ""), []);
});

test("조회 결과가 없어도 편집 중인 프로필에 저장된 모델은 남는다", () => {
  assert.deepEqual(mergeModelOptions([], "  custom-model  "), ["custom-model"]);
});

test("목록에 없는 저장된 모델은 맨 앞에 보존된다", () => {
  assert.deepEqual(mergeModelOptions(["a"], "custom-model"), ["custom-model", "a"]);
});

test("목록에 이미 있는 저장된 모델은 자리를 옮기지 않는다", () => {
  assert.deepEqual(mergeModelOptions(["a", "b"], "b"), ["a", "b"]);
});

test("빈 문자열과 공백 항목은 버린다", () => {
  assert.deepEqual(mergeModelOptions([" ", "a ", ""], ""), ["a"]);
});

test("정적 추천 목록을 내보내지 않는다", () => {
  assert.equal(models.suggestedModels, undefined);
});

test("구독 인증 방식 판단", () => {
  assert.equal(isSubscriptionAuth("claude_oauth"), true);
  assert.equal(isSubscriptionAuth("chatgpt_oauth"), true);
  assert.equal(isSubscriptionAuth("api_key"), false);
});

test("구독 프로필은 모델 없이 저장할 수 있다", () => {
  assert.equal(profileSaveError("내 Claude", "", "claude_oauth"), "");
  assert.equal(profileSaveError("내 ChatGPT", "  ", "chatgpt_oauth"), "");
});

test("구독 프로필도 이름은 있어야 한다", () => {
  assert.equal(profileSaveError(" ", "", "claude_oauth"), "이름을 입력하세요");
});

test("API 키 프로필은 지금처럼 모델이 있어야 저장된다", () => {
  assert.equal(profileSaveError("운영", "", "api_key"), "이름과 모델을 입력하세요");
  assert.equal(profileSaveError("운영", "  ", "api_key"), "이름과 모델을 입력하세요");
  assert.equal(profileSaveError("운영", "gpt-test", "api_key"), "");
});

test("빈 모델은 서버 기본 모델을 기본값으로 표시한다", () => {
  assert.equal(modelLabel("", "gpt-test-default"), "기본값(gpt-test-default)");
  assert.equal(modelLabel("  ", "gpt-test-default"), "기본값(gpt-test-default)");
  assert.equal(modelLabel("", undefined), "기본값");
});

test("지정한 모델은 그대로 표시한다", () => {
  assert.equal(modelLabel(" gpt-test ", "gpt-test-default"), "gpt-test");
});

test("자격 증명이 없으면 구독은 로그인 안내, API 키는 키 안내를 보인다", () => {
  const base = { fetchedCount: 0, error: "", hasCredential: false };
  assert.match(modelsHint({ ...base, isSubscription: true }), /저장한 뒤 로그인하면 쓸 수 있는 모델 목록이 나옵니다/);
  // 이미 저장된 프로필의 로그인이 끊긴 경우에도 맞는 문구여야 하므로 "먼저 모델 없이 저장"을 강요하지 않는다.
  assert.doesNotMatch(modelsHint({ ...base, isSubscription: true }), /먼저 모델 없이 저장/);
  assert.match(modelsHint({ ...base, isSubscription: false }), /API 키를 입력/);
  assert.doesNotMatch(modelsHint({ ...base, isSubscription: true }), /추천/);
});

test("조회 결과가 있으면 개수를, 실패하면 사유를 보인다", () => {
  assert.match(
    modelsHint({ fetchedCount: 3, error: "", hasCredential: true, isSubscription: true }),
    /불러온 모델 3개/,
  );
  assert.match(
    modelsHint({ fetchedCount: 0, error: "HTTP 401", hasCredential: true, isSubscription: false }),
    /HTTP 401/,
  );
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
