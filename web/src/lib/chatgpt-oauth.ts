// ChatGPT 구독 로그인 화면의 순수 로직. 오류 문구는 서버 문구가 아니라 code로 고른다.
// 서버 문구는 바뀔 수 있고, 화면은 사용자가 다음에 할 일을 알려 줘야 하기 때문이다.
import type { ChatGPTDeviceStatus, LLMProfileOAuth } from "./types";

// 서버 오류 code → 화면 문구. 정본 코드 표는 server/chatgpt_login.go의 loginErrorCode다.
export const LOGIN_ERROR_MESSAGES: Record<string, string> = {
  request_too_large: "요청이 너무 큽니다. 붙여넣은 주소를 확인하세요.",
  invalid_request: "요청이 올바르지 않습니다. 콜백 주소를 통째로 붙여넣었는지 확인하세요.",
  callback_url_mismatch:
    "로그인 콜백 주소가 아닙니다. 주소창의 http://localhost:1455/auth/callback?… 주소를 통째로 붙여넣으세요.",
  authorization_denied: "ChatGPT 로그인이 거부됐습니다. 로그인을 다시 시작하세요.",
  credential_key_unavailable: "서버에 자격 증명 암호화 키가 없어 로그인할 수 없습니다. 서버 설정을 확인하세요.",
  profile_lookup_failed: "프로필을 읽지 못했습니다. 잠시 뒤 다시 시도하세요.",
  profile_not_found: "프로필이 없습니다. 목록을 새로 고친 뒤 다시 시도하세요.",
  not_oauth_profile: "이 프로필은 아직 ChatGPT 구독 방식으로 저장되지 않았습니다. 먼저 저장하세요.",
  flow_not_found: "로그인 요청을 찾을 수 없습니다. 로그인을 처음부터 다시 시작하세요.",
  flow_expired: "로그인 시간이 지났습니다. 로그인을 다시 시작하세요.",
  state_mismatch: "다른 로그인 요청의 주소입니다. 이번에 연 로그인 창이 돌려준 주소를 붙여넣으세요.",
  exchange_failed: "ChatGPT 서버와 토큰을 교환하지 못했습니다. 로그인을 다시 시작하세요.",
  network_error: "ChatGPT 서버에 연결하지 못했습니다. 프록시와 네트워크를 확인한 뒤 다시 시작하세요.",
  login_canceled: "로그인이 취소됐습니다. 다시 시작하세요.",
  save_failed: "자격 증명을 저장하지 못했습니다. 잠시 뒤 다시 시도하세요.",
  internal_error: "서버 내부 오류로 로그인을 시작하지 못했습니다. 다시 시도하세요.",
  device_login_disabled:
    "이 ChatGPT 계정은 디바이스 코드 로그인이 꺼져 있습니다. ChatGPT 보안 설정에서 켜거나 콜백 주소 붙여넣기를 쓰세요.",
  device_start_failed: "디바이스 코드를 받지 못했습니다. 프록시와 네트워크를 확인하세요.",
  not_connected: "이미 연결이 해제돼 있습니다.",
  disconnect_failed: "연결을 해제하지 못했습니다. 잠시 뒤 다시 시도하세요.",
};

// code가 없거나 모르는 오류(DB 미연결 503, 네트워크 끊김 등)에 쓴다.
export const UNKNOWN_LOGIN_ERROR = "요청에 실패했습니다. 서버 상태를 확인한 뒤 다시 시도하세요.";

// 디바이스 상태 폴링 간격. 서버 쪽 대기와 맞출 필요는 없고 화면 반응만 보면 된다.
export const DEVICE_POLL_INTERVAL_MS = 3000;

// code 없는 폴링 오류(백엔드 재시작, 네트워크 끊김, DB 미연결 503)를 다시 시도하는 횟수.
// 서버의 흐름은 살아 있으므로 잠깐의 끊김으로 사용자가 코드를 다시 받게 하지 않는다.
export const MAX_POLL_RETRIES = 3;

// errorCode 는 api.ts의 ApiError처럼 code를 실은 오류에서 code를 꺼낸다.
export function errorCode(error: unknown): string | undefined {
  if (typeof error !== "object" || error === null || !("code" in error)) return undefined;
  return typeof error.code === "string" ? error.code : undefined;
}

export function loginErrorMessage(code: string | undefined, provider: "chatgpt" | "claude" = "chatgpt"): string {
  // hasOwn: "toString" 같은 code가 Object.prototype 값을 꺼내지 않게 한다.
  const message = code && Object.hasOwn(LOGIN_ERROR_MESSAGES, code) ? LOGIN_ERROR_MESSAGES[code] : UNKNOWN_LOGIN_ERROR;
  return provider === "claude"
    ? message
        .replaceAll("ChatGPT", "Claude")
        .replaceAll("http://localhost:1455/auth/callback", "http://localhost:53692/callback")
    : message;
}

// deviceFailureMessage 는 폴링이 끝난 상태(failed·expired)의 문구다. expired에는 code가 없을 수 있다.
export function deviceFailureMessage(status: ChatGPTDeviceStatus, code: string | undefined): string {
  if (status === "expired") return LOGIN_ERROR_MESSAGES.flow_expired;
  return loginErrorMessage(code);
}

// shouldRetryPoll 은 폴링 오류 뒤 같은 흐름을 계속 기다릴지 정한다. failedAttempts는 이 오류를 포함한
// 연속 실패 수다. code가 있는 오류(flow_not_found 등)는 서버가 흐름을 끝낸 것이라 다시 시도하지 않는다.
export function shouldRetryPoll(error: unknown, failedAttempts: number): boolean {
  return errorCode(error) === undefined && failedAttempts <= MAX_POLL_RETRIES;
}

export type OAuthConnection = "connected" | "disconnected" | "needs_login";

export function oauthConnection(oauth: LLMProfileOAuth | undefined): OAuthConnection {
  if (!oauth?.connected) return "disconnected";
  return oauth.needs_login ? "needs_login" : "connected";
}
