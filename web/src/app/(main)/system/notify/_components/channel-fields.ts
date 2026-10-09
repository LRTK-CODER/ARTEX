// 알림 채널 필드 표와 설정값 파싱 도구.
//
// 페이지와 나눈 이유는 이것이 뷰가 아니라 **데이터**이기 때문이다. 채널 종류마다 어떤 필드가 있고
// 각각 어떤 컨트롤을 쓰는지, 폼 텍스트와 설정값(JSON) 사이의 양방향 변환을 담는다.
// 파일을 따로 두면 채널을 추가할 때 여기만 고치면 되고 페이지는 바꾸지 않아도 된다.
// 채널 유형의 표시 이름과 소개. 화면 문구에만 영향을 주므로 백엔드가 알 필요가 없어 프런트에 둔다.
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Feishu(Lark)",
  wecom: "WeCom",
  webhook: "범용 Webhook",
  telegram: "Telegram",
  email: "이메일",
};

// 채널별 설정 필드 정의.
//
// 백엔드가 schema를 내려 주게 하지 않고 일부러 프런트에 필드 표를 둔다. 백엔드는
// Validate(필수·형식)만 맡고, UI에 필요한 것은 배치와 컨트롤 유형이라 관심사가 다르다.
// 유일한 결합 지점은 secret_keys다. 어떤 필드를 비밀번호 칸으로 그릴지는 백엔드가 알려 준다.
// 어떤 값이 자격 증명인지는 채널 구현만 알기 때문이다(WeCom은 Webhook 전체가 자격 증명이고,
// DingTalk은 그중 secret 하나뿐이다). 채널을 추가할 때 여기 항목이 빠지면 폼이 비어 보일 뿐
// 조용히 잘못되지는 않는다(아래 hasFields가 알려 준다).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "서명 키",
      kind: "password",
      help: "봇 보안 설정에서 「서명」을 골랐을 때 입력하세요. 「사용자 지정 키워드」를 골랐거나 보안 설정을 켜지 않았으면 비워 두세요",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "서명 검증 키", kind: "password", help: "봇에서 「서명 검증」을 켰을 때 입력하세요. 아니면 비워 두세요" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "대상 URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "요청 메서드",
      kind: "select",
      options: [
        { value: "POST", label: "POST(본문 있음)" },
        { value: "PUT", label: "PUT(본문 있음)" },
        { value: "PATCH", label: "PATCH(본문 있음)" },
        { value: "GET", label: "GET(본문 없음)" },
      ],
    },
    { key: "headers", label: "사용자 지정 요청 헤더", kind: "kv", help: "한 줄에 KEY=VALUE 하나. 예: Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "요청 본문 템플릿",
      kind: "textarea",
      help:
        "비워 두면 내장 기본 템플릿을 씁니다. 변수: {{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}, " +
        "range .Items 안의 .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel. " +
        "문자열을 넣을 때는 {{.Xxx}} 대신 {{json .Xxx}}를 쓰세요. 그러지 않으면 제목의 따옴표가 JSON을 깨뜨립니다.",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API 주소",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "비워 두면 공식 주소를 씁니다. 직접 운영하는 Bot API 리버스 프록시를 쓸 때 입력하세요",
    },
  ],
  email: [
    { key: "host", label: "SMTP 서버", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "포트",
      kind: "number",
      placeholder: "587",
      help: "587은 STARTTLS를 씁니다. 465는 「암시적 TLS」를 켜세요",
    },
    { key: "username", label: "계정", kind: "text" },
    { key: "password", label: "비밀번호 / 앱 비밀번호", kind: "password" },
    { key: "from", label: "보낸 사람", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "받는 사람", kind: "list", help: "여러 주소는 쉼표로 구분하세요" },
    { key: "tls", label: "암시적 TLS", kind: "switch", help: "465 포트는 켜세요. 587은 끈 채로 두세요(STARTTLS를 자동으로 씁니다)" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "제한 없음" },
  { value: "low", label: "낮음 이상" },
  { value: "medium", label: "중간 이상" },
  { value: "high", label: "높음 이상" },
  { value: "critical", label: "치명만" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV는 「한 줄에 KEY=VALUE 하나」 형식의 텍스트 영역을 파싱한다.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs는 쉼표·공백으로 구분한 id 목록을 파싱한다.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords는 줄·쉼표로 구분한 키워드 목록을 파싱한다(취약점 유형 이름에 공백이 있을 수 있어 줄이나 쉼표로 자른다).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
