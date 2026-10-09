// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: 의도 / 커버리지 / 작업 / 심각도는 각각 일관된 색 집합을 가진다.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose는 '치명'에 쓴다 — 꽉 찬 색으로 강하게 강조해, '높음'의 연한 빨강 테두리보다 시각적으로 뚜렷이 높다.
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "할당 대기", tone: "slate" },
  running: { label: "실행 중", tone: "blue" },
  paused: { label: "일시 중지됨", tone: "amber" },
  done: { label: "완료", tone: "green" },
  // blocked = 모델/API/네트워크 장애로 재시도가 소진돼 이 의도는 사실상 탐색하지 못했다(목표에 따른 차단이 아니다).
  blocked: { label: "실행 오류", tone: "red" },
  // exhausted = 단계/시간 한도에 이르러 중간에 끊겨 일부 결과만 기록했다(방향을 다 탐색했다는 뜻이 아니다).
  exhausted: { label: "한도 소진", tone: "violet" },
  // stopped = 과거의 소프트 삭제 상태(보존, 과거 데이터).
  stopped: { label: "중지됨", tone: "slate" },
  // deleted = 사용자가 이 의도를 소프트 삭제했다(노드와 계보는 보존하고, 삭제 원인은 delete_reason 필드 참고).
  deleted: { label: "삭제됨", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "생성됨", tone: "slate" },
  queued: { label: "대기 중", tone: "amber" },
  running: { label: "실행 중", tone: "blue" },
  paused: { label: "일시 중지됨", tone: "amber" },
  done: { label: "완료", tone: "green" },
  failed: { label: "실패", tone: "red" },
  timeout: { label: "시간 초과", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "치명", tone: "rose" },
  high: { label: "높음", tone: "red" },
  medium: { label: "중간", tone: "amber" },
  low: { label: "낮음", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "처리 대기", tone: "amber" },
  in_progress: { label: "처리 중", tone: "blue" },
  confirmed: { label: "확인됨", tone: "red" },
  resolved: { label: "처리됨", tone: "green" },
  fixed: { label: "수정됨", tone: "green" },
  false_positive: { label: "오탐", tone: "slate" },
  ignored: { label: "무시", tone: "neutral" },
  duplicate: { label: "중복", tone: "neutral" },
  risk_accepted: { label: "위험 수용", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "탐색 중", tone: "blue" },
  paused: { label: "일시 중지됨", tone: "amber" },
  stalled: { label: "정체", tone: "red" },
  idle: { label: "유휴", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "진행 중", tone: "blue" },
  met: { label: "달성됨", tone: "green" },
  abandoned: { label: "포기됨", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "허용", tone: "green" },
  block: { label: "차단", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "관측", tone: "slate" },
  confirmed: { label: "확인", tone: "green" },
  tombstoned: { label: "폐기", tone: "neutral" },
};

// 알림 전달 상태. sending에 amber가 아니라 blue를 쓰는 것은 '문제가 있다'가 아니라
// '이미 할당받아 보내는 중'이라는 뜻이라, pending의 대기 의미와 구분되어야 하기 때문이다.
const delivery: Record<string, StatusMeta> = {
  pending: { label: "발송 대기", tone: "amber" },
  sending: { label: "발송 중", tone: "blue" },
  sent: { label: "전달됨", tone: "green" },
  failed: { label: "실패", tone: "red" },
  skipped: { label: "건너뜀", tone: "neutral" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}
