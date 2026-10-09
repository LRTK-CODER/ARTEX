import {
  deviceFailureMessage,
  errorCode,
  LOGIN_ERROR_MESSAGES,
  loginErrorMessage,
  MAX_POLL_RETRIES,
  oauthConnection,
  shouldRetryPoll,
  UNKNOWN_LOGIN_ERROR,
} from "./chatgpt-oauth.ts";
import assert from "node:assert/strict";
import test from "node:test";

// 서버 loginErrorCode 전부. 서버에 코드가 늘면 여기와 문구 표를 함께 고친다.
const serverCodes = [
  "request_too_large",
  "invalid_request",
  "callback_url_mismatch",
  "authorization_denied",
  "credential_key_unavailable",
  "profile_lookup_failed",
  "profile_not_found",
  "not_oauth_profile",
  "flow_not_found",
  "flow_expired",
  "state_mismatch",
  "exchange_failed",
  "network_error",
  "login_canceled",
  "save_failed",
  "internal_error",
  "device_login_disabled",
  "device_start_failed",
  "not_connected",
  "disconnect_failed",
];

test("every server error code has its own screen message", () => {
  for (const code of serverCodes) {
    const message = loginErrorMessage(code);
    assert.notEqual(message, UNKNOWN_LOGIN_ERROR, code);
    assert.equal(message, LOGIN_ERROR_MESSAGES[code]);
  }
  assert.deepEqual(Object.keys(LOGIN_ERROR_MESSAGES).sort(), [...serverCodes].sort());
});

test("missing or unknown codes fall back to the generic message", () => {
  for (const code of [undefined, "", "toString", "something_new"]) {
    assert.equal(loginErrorMessage(code), UNKNOWN_LOGIN_ERROR, String(code));
  }
});

test("Claude 오류는 제공자와 콜백 주소를 Claude에 맞춘다", () => {
  assert.match(loginErrorMessage("not_oauth_profile", "claude"), /Claude/);
  assert.match(loginErrorMessage("callback_url_mismatch", "claude"), /localhost:53692\/callback/);
  assert.doesNotMatch(loginErrorMessage("callback_url_mismatch", "claude"), /1455/);
  assert.equal(loginErrorMessage("unknown", "claude"), UNKNOWN_LOGIN_ERROR);
});

test("errorCode reads only string codes", () => {
  const withCode = Object.assign(new Error("서버 문구"), { code: "flow_expired" });
  assert.equal(errorCode(withCode), "flow_expired");
  assert.equal(errorCode(new Error("x")), undefined);
  assert.equal(errorCode({ code: 503 }), undefined);
  assert.equal(errorCode(null), undefined);
  assert.equal(errorCode("flow_expired"), undefined);
});

test("expired device flow without a code still says the login timed out", () => {
  assert.equal(deviceFailureMessage("expired", undefined), LOGIN_ERROR_MESSAGES.flow_expired);
  assert.equal(deviceFailureMessage("failed", "network_error"), LOGIN_ERROR_MESSAGES.network_error);
  assert.equal(deviceFailureMessage("failed", undefined), UNKNOWN_LOGIN_ERROR);
});

test("connection state distinguishes connected, disconnected and needs login", () => {
  const cases = [
    [undefined, "disconnected"],
    [{ connected: false, needs_login: false }, "disconnected"],
    // 연결이 안 됐으면 needs_login 값과 무관하게 연결 안 됨이다.
    [{ connected: false, needs_login: true }, "disconnected"],
    [{ connected: true, needs_login: false, plan: "plus" }, "connected"],
    [{ connected: true, needs_login: true }, "needs_login"],
  ];
  for (const [oauth, want] of cases) {
    assert.equal(oauthConnection(oauth), want, JSON.stringify(oauth));
  }
});

test("poll errors without a code are retried a few times, coded errors stop at once", () => {
  const transient = new Error("POST /llm/oauth/chatgpt/device/x: 503");
  for (let attempt = 1; attempt <= MAX_POLL_RETRIES; attempt++) {
    assert.equal(shouldRetryPoll(transient, attempt), true, `attempt ${attempt}`);
  }
  assert.equal(shouldRetryPoll(transient, MAX_POLL_RETRIES + 1), false);
  const gone = Object.assign(new Error("서버 문구"), { code: "flow_not_found" });
  assert.equal(shouldRetryPoll(gone, 1), false);
});
