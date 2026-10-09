"use client";

// LLM 재시도 설정의 공용 부품: 다섯 단계 재시도 각각의 「횟수 + 간격」.
//
// 다섯 단계는 안쪽부터: 연결(SDK) → 빈 응답(SDK) → 같은 제공자 안전 구간 → 장애 조치 회로 차단기 → 탐색 의도 재실행.
// 앞의 세 단계는 엔드포인트에 따라 달라지므로 LLM 프로필마다 전역 기본값을 덮어쓸 수 있다. 뒤의 두 단계는 프로세스 단위라 전역에 하나뿐이다.
//
// 모든 입력은 백엔드 db.RetryRule과 같은 「비워 두면 설정 안 함」 의미를 따른다:
//   횟수   비움/0 = 내장 기본값 | -1 = 이 단계 재시도 끄기 | >0 = 이 횟수 사용
//   간격   비움/0 = 이 단계의 원래 지수 백오프 | >0 = 이 고정 간격(밀리초) 사용

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** 이 단계 재시도가 어디서, 무엇이 실행하는지 */
  where: string;
  /** 어떤 오류가 이 단계로 오는지. 짐작하지 않게 상태 코드까지 적는다 */
  trigger: string;
  /** 비슷해 보이지만 이 단계로 **오지 않는** 오류. 값을 넣었는데 반응이 없어 버그로 오해하지 않게 한다 */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 횟수를 비웠을 때의 기본값. 자리표시자에 쓴다 */
  defAttempts: number;
  /** 간격을 비웠을 때의 기본 방식. 자리표시자에 쓴다 */
  defInterval: string;
  /** 횟수에 -1을 넣었을 때의 뜻 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "연결 재시도",
    where: "SDK · 200을 받기 전",
    trigger:
      "연결하지 못했거나 아직 200을 받지 못한 경우: 연결 재설정 / 읽기·쓰기 시간 초과 / DNS 실패 같은 네트워크 계층 오류와 HTTP 408, 429, 500, 502, 503, 504.",
    skips:
      "나머지 상태 코드(400 / 401 / 403 / 404 / 413 / 422 등)는 확정적인 거부라 다시 보내도 똑같이 실패하므로 바로 위로 올립니다.",
    desc: "같은 요청을 그대로 다시 보냅니다. 스트림이 시작된 뒤(200을 받은 뒤)에 끊기면 이 단계가 맡지 않습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s 지수(최대 8s)",
    offHint: "-1 = 한 번도 재시도하지 않고 실패를 바로 위로 올림",
  },
  empty: {
    title: "빈 응답 재시도",
    where: "SDK · openai 형식만",
    trigger:
      "HTTP 200이고 finish_reason도 정상 stop인데 응답 전체에 콘텐츠 블록이 하나도 없는 경우. 게이트웨이의 빈 프레임, 사고(thinking) 필드 프레임 누락, 일시적인 샘플링 오류가 이렇게 보입니다.",
    skips:
      "max_tokens에 잘려 내용이 없는 경우는 해당하지 않습니다(출력 최대 토큰을 올려야 해결되고, 다시 보내도 또 잘립니다).",
    desc: "프롬프트 전체를 다시 보내므로 긴 컨텍스트에서는 비쌉니다. 횟수를 크게 잡지 마세요.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s 지수(최대 8s)",
    offHint: "-1 = 빈 응답을 그대로 넘김",
  },
  stream: {
    title: "같은 제공자 안전 구간 재시도",
    where: "ARTEX · 출력을 넘기기 전",
    trigger:
      "스트림이 열린 뒤(200을 받은 뒤) 생긴 문제: 연결이 중간에 끊김, 제공자 overloaded, 스트림 안의 429 / 5xx 오류 이벤트. 단, 호출한 쪽에 토큰을 하나도 넘기지 않았을 때만입니다.",
    skips:
      "사용 한도 소진(402 / insufficient_quota, 다른 프로필로 장애 조치), 컨텍스트 초과(413 / context length, 압축으로 처리), 400 / 401 / 403 / 404 / 422 확정적 거부는 모두 재시도하지 않습니다.",
    desc: "같은 프로필로 같은 요청을 다시 보냅니다. 아직 아무 출력도 넘기지 않았으므로 다시 보내도 모델 출력이나 도구 실행이 중복되지 않습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 2,
    defInterval: "0.5s→1s 지수(최대 4s)",
    offHint: "-1 = 스트림이 끊기면 바로 바깥 단계의 탐색 의도 재실행에 넘김",
  },
  breaker: {
    title: "장애 조치 회로 차단기",
    where: "ARTEX · 프로세스 단위, 전역에 하나",
    trigger:
      "일시적 실패(429, 5xx, 네트워크 오류)가 연속으로 임계값에 이르면 차단합니다. 잔액 부족(402), 키 무효(401 / 403), 모델 없음(404) 같은 확정적 실패는 임계값과 상관없이 처음부터 차단합니다.",
    skips: "한 번 성공하면 0으로 돌아가므로, 가끔 실패하는 프로필이 조금씩 쌓여 차단되지는 않습니다.",
    desc: "차단되면 대기 시간에 들어가고, 그동안 장애 조치는 이 프로필을 건너뜁니다. 상태는 DB에 저장되어 재시작해도 남습니다.",
    attemptsLabel: "차단까지 연속 실패 횟수",
    defAttempts: 3,
    defInterval: "1min→5min→30min 단계",
    offHint: "-1 = 일시적 실패로는 차단하지 않음(확정적 실패는 그대로 차단)",
  },
  intent: {
    title: "탐색 의도 재실행",
    where: "ARTEX · 프로세스 단위, 전역에 하나",
    trigger:
      "앞 단계들이 모두 막지 못해 워커가 model_error로 끝난 경우. 안쪽 재시도를 다 썼거나, 출력을 넘기기 시작한 뒤에 스트림이 끊긴 경우입니다(그때는 다시 보내기가 안전하지 않아 처음부터 다시 해야 합니다).",
    skips:
      "사용 한도 소진은 장애 조치가 이미 처리하므로 여기서 재실행하지 않습니다. 작업이 일시 중지 / 종료 / 마무리에 들어가면 바로 물러나고 백오프 시간을 쓰지 않습니다.",
    desc: "탐색 의도 하나를 처음부터 다시 실행합니다. 가장 바깥 단계라서 한 번 재실행하면 안쪽 단계의 횟수가 다시 곱해집니다.",
    attemptsLabel: "재실행 횟수",
    defAttempts: 2,
    defInterval: "고정 3s",
    offHint: "-1 = 재실행하지 않고 그 탐색 의도를 바로 blocked로 판정",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** 밀리초를 읽기 쉽게 바꾼다. 입력 칸 옆에 보여 줘서 0의 개수를 세지 않게 한다. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 제어되는 숫자 입력: 빈 문자열 ↔ 0. 입력 중간 상태("-", "1e")는 로컬에만 두고 부모에 알리지 않는다. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 부모가 값 묶음을 통째로 바꿀 때(정책을 읽어 옴, 프로필 전환) 따라간다. 직접 입력할 때는 여기 오지 않는다.
  // 그때 value는 이미 로컬 텍스트를 파싱한 결과와 같기 때문이다.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 재시도 한 단계의 두 입력 칸. idPrefix는 한 페이지에 여러 번 나올 때 label의 htmlFor가 겹치지 않게 한다. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = 프로필 편집 패널의 간단한 형태: 자세한 설명은 빼고 「어떤 오류가 이 단계로 오는지」 한 줄만 남긴다 */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* 어떤 오류가 이 단계로 오는지 상태 코드까지 적는다. 값을 넣었는데 효과가 없다면 대개 그 오류가 이 단계로 오지 않는 것이다. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">조건</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">해당하지 않음</span>: {meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`기본값 ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            간격 ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="기본 백오프"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `고정 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">비워 두면 기본값을 씁니다. {meta.offHint}.</p>}
    </div>
  );
}

/** 프로필 편집 패널에서 덮어쓰는 세 단계(엔드포인트에 따라 달라지는 세 단계). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">재시도 덮어쓰기</Label>
        <p className="text-muted-foreground text-xs">
          이 프로필에만 적용되고 「재시도와 백오프」의 전역 기본값을 덮어씁니다. 칸을 비우면 전역 값을 따르고, 횟수에
          -1을 넣으면 이 단계 재시도를 끕니다. 간격을 넣으면 지수 백오프 대신 고정 간격을 씁니다. 회로 차단기와 탐색
          의도 재실행은 프로세스 단위라 전역 설정 탭에서만 바꿀 수 있습니다.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「재시도와 백오프」 탭: 다섯 단계의 전역 기본값. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`재시도 정책을 읽지 못했습니다: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // 백엔드가 범위를 벗어난 값을 범위 안으로 맞춰 돌려주므로, 돌려받은 값으로 갱신해 화면과 저장값을 같게 한다.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("저장했습니다. 바로 적용됩니다(지금 진행 중인 호출은 이전 값을 씁니다)");
    } catch (e) {
      toast.error(`저장하지 못했습니다: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> 재시도 정책을 읽는 중…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        모델 호출 한 번의 실패는 다섯 단계 재시도를 안쪽부터 차례로 거칩니다:
        <span className="text-foreground">
          {" "}
          연결 → 빈 응답 → 같은 제공자 안전 구간 → 장애 조치 회로 차단기 → 탐색 의도 재실행
        </span>
        . 안쪽 단계를 다 써야 바깥 단계로 넘어가므로 횟수는 <span className="text-foreground">곱해집니다</span>. 모든
        단계를 최대로 잡으면 한 번의 일시 장애로 요청이 수십 번 나갈 수 있습니다. 모두 비워 두면 현재 기본값이고, 이
        페이지가 없을 때와 똑같이 동작합니다. 앞의 세 단계는 LLM 프로필마다 따로 덮어쓸 수 있습니다.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          저장
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          모두 기본값으로 되돌리기
        </Button>
      </div>
    </div>
  );
}
