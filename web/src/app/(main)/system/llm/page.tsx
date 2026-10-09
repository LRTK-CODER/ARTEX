"use client";

import * as React from "react";

import {
  Loader2Icon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  SaveIcon,
  StarIcon,
  Trash2Icon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import { oauthConnection } from "@/lib/chatgpt-oauth";
import type { LLMAuthType, LLMPoolMember, LLMPoolStatus, LLMProfile, LLMRetryOverride } from "@/lib/types";
import { cn } from "@/lib/utils";

import { SubscriptionAccount } from "./_components/chatgpt-login";
import { ProfileRetryFields, RetryPolicyPanel, ZERO_OVERRIDE } from "./_components/retry";

// 사고 스위치(thinking.type)와 사고 강도(reasoning_effort)는 **서로 독립된** 필드라 따로 설정한다.
// thinking 필드 없이 강도 파라미터만으로 사고를 켜는 API가 있어서 둘을 떼어 놓는다.
// DB의 빈 문자열 = 그 필드를 **보내지 않음**. Radix Select는 빈 value를 받지 않으므로 UI는 "none"
// 표지값으로 "보내지 않음"을 나타내고, 저장·읽기 때 ""와 서로 바꾼다(NONE / fromStore / toStore).
const NONE = "none";
const fromStore = (v?: string) => (v ? v : NONE);
const toStore = (v: string) => (v === NONE ? "" : v);
const THINKING_TYPES: { value: string; label: string }[] = [
  { value: NONE, label: "보내지 않음(기본값)" },
  { value: "disabled", label: "끄기" },
  { value: "enabled", label: "켜기" },
];
// 출력 최대 토큰을 어떤 요청 필드 이름으로 보낼지(openai 형식에서만 의미가 있다). NONE ↔ ""는 같은 표지값 변환을 쓴다.
const MAX_TOKENS_FIELDS: { value: string; label: string }[] = [
  { value: NONE, label: "max_tokens(기본값)" },
  { value: "max_completion_tokens", label: "max_completion_tokens" },
];
// 나머지 두 형식은 필드 이름이 고정이라 이 선택이 의미가 없으므로, 설명 문구에 그렇게 적는다.
const MAX_TOKENS_FIELD_HINTS: Record<string, string> = {
  openai:
    "최대 토큰을 어느 키로 보낼지 고릅니다. 기본값은 max_tokens이고 호환 게이트웨이는 대부분 이것만 받습니다. 반대로 OpenAI 공식 추론 모델(o 시리즈 / GPT-5)은 max_completion_tokens만 받고, max_tokens를 받으면 바로 unsupported_parameter 오류를 냅니다.",
  anthropic: "openai 형식에서만 고를 수 있습니다. Anthropic의 필드 이름은 max_tokens로 고정입니다.",
  "openai-responses": "openai 형식에서만 고를 수 있습니다. Responses API의 필드 이름은 max_output_tokens로 고정입니다.",
};
const EFFORT_LEVELS: { value: string; label: string }[] = [
  { value: NONE, label: "보내지 않음(기본값)" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];

function cooldownText(secs: number) {
  if (secs <= 0) return "";
  if (secs < 60) return `${secs}s`;
  return `${Math.ceil(secs / 60)}min`;
}

// 카드에 보여 줄 프로필의 「정상 여부」. 키가 없는 프로필은 요청을 아예 보낼 수 없으므로 차단보다 먼저 알린다.
// 나머지 상태는 장애 조치의 회로 차단기 기록에서 온다(장애 조치가 꺼져 있으면 새 기록이 생기지 않으므로 그때 「정상」 = 알려진 장애 없음).
type Health = { label: string; cls: string; hint?: string };
function healthOf(p: LLMProfile, m?: LLMPoolMember): Health {
  // 구독 프로필은 키가 없으므로 키 대신 로그인 상태를 먼저 본다.
  const connection =
    p.auth_type === "chatgpt_oauth" || p.auth_type === "claude_oauth" ? oauthConnection(p.oauth) : null;
  if (connection === "disconnected") {
    return {
      label: `${p.auth_type === "claude_oauth" ? "Claude" : "ChatGPT"} 연결 안 됨`,
      cls: "border-muted-foreground/40 text-muted-foreground",
    };
  }
  if (connection === "needs_login") {
    return { label: "재로그인 필요", cls: "border-amber-500/50 text-amber-600 dark:text-amber-400" };
  }
  if (!connection && !p.api_key_hint) {
    return {
      label: "키 설정 안 됨",
      cls: "border-muted-foreground/40 text-muted-foreground",
      hint: "API 키가 없어 호출할 수 없습니다",
    };
  }
  if (m?.state === "tripped") {
    return {
      label: m.cooldown_secs > 0 ? `차단됨 · ${cooldownText(m.cooldown_secs)}` : "차단됨",
      cls: "border-destructive/50 text-destructive",
      hint: m.last_error,
    };
  }
  if (m?.state === "degraded") {
    return {
      label: `이상 · ${m.fails}회 실패`,
      cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
      hint: m.last_error,
    };
  }
  return { label: "정상", cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400" };
}

// ─────────────────────────────────────────────────────────────────────────────
// 장애 조치 설정 패널
// ─────────────────────────────────────────────────────────────────────────────

function PoolSheet({
  open,
  onOpenChange,
  pool,
  onReload,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  pool: LLMPoolStatus | null;
  onReload: () => Promise<void>;
}) {
  const [busy, setBusy] = React.useState(false);

  // 대기 시간 카운트다운은 백엔드가 계산한 남은 초다. 패널이 열려 있고 정상이 아닌 프로필이 있을 때만 주기적으로 가져와 줄어드는 것을 보여 준다.
  React.useEffect(() => {
    if (!open || !pool?.enabled || !pool.chain.some((m) => m.state !== "ok")) return;
    const t = setInterval(() => void onReload(), 10_000);
    return () => clearInterval(t);
  }, [open, pool, onReload]);

  async function toggle(patch: { llm_pool_enabled?: boolean; llm_pool_bind_fallback?: boolean }) {
    if (busy) return;
    setBusy(true);
    try {
      await api.setSettings(patch);
      await onReload();
      if (patch.llm_pool_enabled !== undefined) {
        toast.success(patch.llm_pool_enabled ? "LLM 장애 조치를 켰습니다" : "LLM 장애 조치를 껐습니다");
      } else {
        toast.success("대체 설정을 업데이트했습니다");
      }
    } catch (e) {
      toast.error(`설정하지 못했습니다: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function recover(id?: string) {
    try {
      await api.resetLLMPool(id);
      await onReload();
      toast.success(id ? "이 프로필을 복구했습니다" : "모든 프로필을 복구했습니다");
    } catch (e) {
      toast.error(`복구하지 못했습니다: ${(e as Error).message}`);
    }
  }

  const enabled = pool?.enabled ?? false;
  const chain = pool?.chain ?? [];
  // 장애 조치에 참여하는 프로필(「장애 조치 제외」로 표시한 것은 뺀다). 순서는 백엔드가 실제로 시도하는 순서다.
  const inChain = chain.filter((m) => m.active || !m.excluded);
  const tripped = chain.filter((m) => m.state === "tripped");

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex flex-col gap-0 p-0 data-[side=right]:sm:max-w-lg">
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            <ZapIcon className="size-4" /> LLM 장애 조치
          </SheetTitle>
          <SheetDescription>
            켜면 <b>모델을 지정하지 않은</b> 에이전트는 현재 프로필을 쓸 수 없을 때(잔액 부족 / 키 무효 / 속도 제한 /
            서비스 장애) 다음 프로필로 자동 전환합니다.
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-6">
          <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="grid gap-0.5">
              <Label className="text-sm">장애 조치 사용</Label>
              <p className="text-muted-foreground text-xs">
                기본값은 꺼짐입니다. 꺼져 있으면 항상 활성 프로필만 쓰고, 실패하면 그대로 실패합니다.
              </p>
            </div>
            <Switch
              checked={enabled}
              disabled={busy}
              onCheckedChange={(v) => void toggle({ llm_pool_enabled: v })}
              aria-label="LLM 장애 조치 스위치"
            />
          </div>

          {enabled && (
            <>
              <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">지정한 모델이 실패해도 대체</Label>
                  <p className="text-muted-foreground text-xs">
                    기본값은 꺼짐입니다. 에이전트나 작업이 프로필을 지정하면 그것만 쓰고, 실패하면 그대로
                    실패합니다(몰래 다른 모델로 바꾸지 않습니다). 켜면 지정한 프로필이 실패해도 아래 장애 조치 순서로
                    넘어갑니다.
                  </p>
                </div>
                <Switch
                  checked={pool?.bind_fallback ?? false}
                  disabled={busy}
                  onCheckedChange={(v) => void toggle({ llm_pool_bind_fallback: v })}
                  aria-label="지정 프로필 실패 시 대체 스위치"
                />
              </div>

              <Separator />

              <div className="grid gap-2">
                <div className="flex items-center justify-between">
                  <Label className="text-sm">장애 조치 순서</Label>
                  {tripped.length > 0 && (
                    <Button size="sm" variant="ghost" onClick={() => void recover()}>
                      <RotateCcwIcon /> 모두 복구
                    </Button>
                  )}
                </div>
                {inChain.length < 2 && (
                  <p className="text-muted-foreground text-xs">
                    지금 쓸 수 있는 프로필이 {inChain.length}개뿐이라 장애 조치가 동작하지 않습니다. API 키가 있고 장애
                    조치에 참여하는 프로필이 2개 이상 필요합니다.
                  </p>
                )}
                {chain.map((m) => {
                  const excluded = m.excluded && !m.active;
                  const order = excluded ? null : inChain.findIndex((x) => x.profile_id === m.profile_id) + 1;
                  return (
                    <div
                      key={m.profile_id}
                      className={cn(
                        "grid gap-1 rounded-lg border p-2.5 text-sm",
                        excluded && "opacity-55",
                        m.state === "tripped" && "border-destructive/40",
                      )}
                    >
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="w-5 shrink-0 text-center font-mono text-muted-foreground text-xs">
                          {order ?? "—"}
                        </span>
                        <span className="font-medium">{m.name}</span>
                        {m.active && (
                          <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                            활성
                          </Badge>
                        )}
                        {excluded && <Badge variant="outline">장애 조치 제외</Badge>}
                        <div className="ml-auto flex items-center gap-2">
                          {m.state === "tripped" && m.cooldown_secs > 0 && (
                            <span className="text-muted-foreground text-xs">대기 {cooldownText(m.cooldown_secs)}</span>
                          )}
                          {m.state === "degraded" && (
                            <span className="text-muted-foreground text-xs">연속 {m.fails}회 실패</span>
                          )}
                          {m.state !== "ok" && (
                            <Button
                              size="icon"
                              variant="ghost"
                              className="size-7"
                              aria-label="지금 복구"
                              title="지금 복구: 차단을 풀고 다음 호출에서 이 프로필을 다시 시도합니다"
                              onClick={() => void recover(m.profile_id)}
                            >
                              <RotateCcwIcon className="size-3.5" />
                            </Button>
                          )}
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-x-3 pl-7 text-muted-foreground text-xs">
                        <code className="truncate font-mono">{m.model}</code>
                        {!m.active && <span>우선순위 {m.priority}</span>}
                      </div>
                      {m.last_error && (
                        <p className="truncate pl-7 font-mono text-muted-foreground text-xs" title={m.last_error}>
                          {m.last_error}
                        </p>
                      )}
                    </div>
                  );
                })}
                {chain.length === 0 && (
                  <div className="rounded-lg border border-dashed p-4 text-center text-muted-foreground text-sm">
                    프로필 없음
                  </div>
                )}
              </div>

              <div className="rounded-lg border border-dashed p-3 text-muted-foreground text-xs leading-relaxed">
                활성 프로필은 항상 1순위이고, 나머지는 우선순위가 높은 것부터입니다(각 프로필에서 설정). 실패한 프로필은
                대기 시간(60s → 5min → 30min)에 들어가 그동안 건너뛰고, 회복되면 자동으로 다시 씁니다. 컨텍스트 창에
                현재 요청이 들어가지 않는 프로필도 건너뜁니다. 모델을 지정한 에이전트와 작업은 기본적으로 장애 조치에
                참여하지 않습니다.
              </div>
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// LLM 프로필 편집 패널(새로 만들기와 편집이 같은 폼을 쓴다)
// ─────────────────────────────────────────────────────────────────────────────

function ProfileSheet({
  profile,
  open,
  onOpenChange,
  onSaved,
}: {
  profile: LLMProfile | null; // null = 새로 만들기
  open: boolean;
  onOpenChange: (o: boolean) => void;
  // needsLogin: 새로 만든 구독 프로필이라 바로 로그인 화면을 다시 열어야 한다.
  onSaved: (id: string, needsLogin: boolean) => void;
}) {
  const isNew = !profile;
  const [authType, setAuthType] = React.useState<LLMAuthType>("api_key");
  const [name, setName] = React.useState("");
  const [format, setFormat] = React.useState<"anthropic" | "openai" | "openai-responses">("anthropic");
  const [model, setModel] = React.useState("");
  const [baseUrl, setBaseUrl] = React.useState("");
  const [proxy, setProxy] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [keyHint, setKeyHint] = React.useState("");
  const [rps, setRps] = React.useState("0");
  const [rpm, setRpm] = React.useState("0");
  const [cw, setCw] = React.useState("0"); // 컨텍스트 창(K 토큰). 0 = 기본값 200K
  const [thinkingType, setThinkingType] = React.useState(NONE);
  const [effort, setEffort] = React.useState(NONE);
  const [priority, setPriority] = React.useState("0"); // 장애 조치 순위. 클수록 먼저
  const [poolExclude, setPoolExclude] = React.useState(false);
  const [streaming, setStreaming] = React.useState(true); // true = 스트리밍(기본값), false = 비스트리밍
  const [maxTokens, setMaxTokens] = React.useState("0"); // 응답 한 번의 출력 최대 토큰. 0 = 보내지 않음
  const [maxTokensField, setMaxTokensField] = React.useState(NONE); // 최대 토큰을 보낼 필드 이름. NONE = max_tokens
  const [sessionHeaderKey, setSessionHeaderKey] = React.useState(""); // 사용자 지정 세션 헤더 이름. 비어 있으면 보내지 않음
  const [retry, setRetry] = React.useState<LLMRetryOverride>(ZERO_OVERRIDE); // 이 프로필의 재시도 덮어쓰기. 모두 0이면 전역 값을 따름
  const [testing, setTesting] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [models, setModels] = React.useState<string[]>([]);
  const [loadingModels, setLoadingModels] = React.useState(false);
  const [modelsOpen, setModelsOpen] = React.useState(false);

  // 열 때마다 받은 profile로 폼을 다시 채운다(새로 만들기면 기본값으로 초기화). 패널을 닫았다 열면
  // 깨끗하게 새로 시작하고, 이전 프로필의 값이 남지 않는다.
  React.useEffect(() => {
    if (!open) return;
    setName(profile?.name ?? "");
    setAuthType(profile?.auth_type ?? "api_key");
    setFormat(profile?.format === "openai" || profile?.format === "openai-responses" ? profile.format : "anthropic");
    setModel(profile?.model ?? "");
    setBaseUrl(profile?.base_url ?? "");
    setProxy(profile?.proxy ?? "");
    setRps(String(profile?.rate_per_second ?? 0));
    setRpm(String(profile?.rate_per_minute ?? 0));
    setCw(String(profile?.context_window_k ?? 0));
    setThinkingType(fromStore(profile?.thinking_type));
    setEffort(fromStore(profile?.reasoning_effort));
    setPriority(String(profile?.priority ?? 0));
    setPoolExclude(profile?.pool_exclude ?? false);
    setStreaming(profile?.streaming ?? true);
    setMaxTokens(String(profile?.max_tokens ?? 0));
    setMaxTokensField(fromStore(profile?.max_tokens_field));
    setSessionHeaderKey(profile?.session_header_key ?? "");
    setRetry(profile?.retry ?? ZERO_OVERRIDE);
    setApiKey("");
    setKeyHint(profile?.api_key_hint ?? "");
    setModels([]);
    setModelsOpen(false);
  }, [open, profile]);

  const profileId = profile ? Number(profile.id) : undefined;
  // 제공자별 서버 규칙과 같은 형식을 보내며 ChatGPT만 스트리밍으로 고정한다.
  const isChatGPT = authType === "chatgpt_oauth";
  const isClaude = authType === "claude_oauth";
  const isOAuth = isChatGPT || isClaude;
  const subscriptionFormat = isClaude ? "anthropic" : "openai-responses";
  const effectiveFormat = isOAuth ? subscriptionFormat : format;
  // 로그인·해제 뒤 카드 목록만 새로 읽는다. 참조가 바뀌면 디바이스 폴링이 다시 걸리므로 고정한다.
  const handleAccountChanged = React.useCallback(() => onSaved(profile?.id ?? "", false), [onSaved, profile]);

  async function loadModels() {
    if (loadingModels) return;
    setLoadingModels(true);
    setModels([]);
    try {
      const r = await api.fetchLLMModels(effectiveFormat, baseUrl, apiKey, proxy, profileId, authType);
      if (r.ok && r.models && r.models.length > 0) {
        setModels(r.models);
        setModelsOpen(true);
        toast.success(`모델 ${r.models.length}개를 불러왔습니다`);
      } else {
        toast.error(`모델을 불러오지 못했습니다: ${r.error ?? "받은 모델이 없습니다"}`);
      }
    } catch (e) {
      toast.error(`모델을 불러오다 오류가 났습니다: ${(e as Error).message}`);
    } finally {
      setLoadingModels(false);
    }
  }

  async function testConnection() {
    if (testing) return;
    setTesting(true);
    try {
      // 프로필이 실제로 쓸 사고 파라미터로 테스트한다. 그래야 그 필드를 지원하지 않는 모델이 작업을 돌릴 때가 아니라
      // 여기서 실패한다. profile id를 넘기므로 키 입력 칸이 비어 있으면 저장된 키를 쓴다.
      const r = await api.testLLM(
        effectiveFormat,
        model,
        baseUrl,
        apiKey,
        proxy,
        toStore(thinkingType),
        toStore(effort),
        profileId,
        isChatGPT || streaming,
        sessionHeaderKey.trim(),
        authType,
      );
      // 응답 내용도 함께 보여 준다. 모델이 실제로 답한 것을 봐야 대화에서 동작하는 것과 같다고 할 수 있다.
      if (r.ok)
        toast.success(`연결 성공 · ${r.latency_ms ?? "?"}ms · ${r.model ?? model}`, {
          description: r.reply ? `응답: ${r.reply}` : undefined,
        });
      else toast.error(`연결하지 못했습니다: ${r.error ?? "알 수 없는 오류"}`);
    } catch (e) {
      toast.error(`테스트하다 오류가 났습니다: ${(e as Error).message}`);
    } finally {
      setTesting(false);
    }
  }

  async function save() {
    if (!name.trim() || !model.trim()) {
      toast.error("이름과 모델을 입력하세요");
      return;
    }
    if (saving) return;
    setSaving(true);
    try {
      const { id } = await api.saveLLMProfile({
        ...(profile ? { id: Number(profile.id) } : {}),
        name: name.trim(),
        format: effectiveFormat,
        model: model.trim(),
        base_url: isOAuth ? "" : baseUrl.trim(),
        proxy: proxy.trim(),
        api_key: isOAuth ? "" : apiKey,
        rate_per_second: Number(rps) || 0,
        rate_per_minute: Number(rpm) || 0,
        context_window_k: Number(cw) || 0,
        thinking_type: toStore(thinkingType),
        reasoning_effort: toStore(effort),
        priority: Number(priority) || 0,
        pool_exclude: poolExclude,
        streaming: isChatGPT || streaming,
        max_tokens: Math.max(0, Number(maxTokens) || 0),
        // 필드 이름 선택은 openai(Chat Completions)에서만 의미가 있고, 다른 형식은 모두 기본값으로 돌린다.
        // 백엔드도 같은 정규화를 한 번 더 하지만, UI가 앞뒤가 맞지 않는 값을 보내지 않게 여기서도 한다.
        max_tokens_field: effectiveFormat === "openai" ? toStore(maxTokensField) : "",
        session_header_key: sessionHeaderKey.trim(),
        retry,
        auth_type: authType,
      });
      // 로그인 API는 저장된 구독 프로필에만 열린다. 방식을 바꿔 저장했으면 화면을 다시 열어 로그인하게 한다.
      const needsLogin = isOAuth && profile?.auth_type !== authType;
      if (needsLogin)
        toast.success(`저장했습니다: ${name.trim()}. 이제 ${isClaude ? "Claude" : "ChatGPT"}에 로그인하세요`);
      else if (isNew) toast.success(`만들었습니다: ${name.trim()}(카드에서 「활성으로 설정」을 눌러 사용하세요)`);
      else
        toast.success(
          profile?.is_default ? "저장했습니다. 활성 프로필이라 재시작 없이 바로 적용됩니다" : "저장했습니다",
        );
      onSaved(String(id), needsLogin);
      onOpenChange(false);
    } catch (e) {
      toast.error(`저장하지 못했습니다: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:min-w-[420px] data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            {isNew ? "새 LLM 프로필" : `편집: ${profile?.name}`}
            {profile?.is_default && (
              <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                활성
              </Badge>
            )}
          </SheetTitle>
          <SheetDescription>
            {isNew
              ? "만든 뒤 자동으로 활성화되지 않습니다. 카드에서 「활성으로 설정」을 눌러 사용하세요."
              : "바꾼 뒤 저장을 누르세요. 활성 프로필은 저장하면 모든 에이전트에 바로 적용됩니다."}
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="p-name">이름</Label>
              <Input id="p-name" placeholder="예: OpenAI 운영" value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label>인증 방식</Label>
              <Select value={authType} onValueChange={(v) => setAuthType(v as LLMAuthType)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="api_key">API Key</SelectItem>
                  <SelectItem value="chatgpt_oauth">ChatGPT 구독</SelectItem>
                  <SelectItem value="claude_oauth">Claude 구독</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          {isOAuth ? (
            <SubscriptionAccount
              key={`${profile?.id ?? "new"}-${authType}`}
              profile={profile}
              onChanged={handleAccountChanged}
              provider={isClaude ? "claude" : "chatgpt"}
            />
          ) : (
            <div className="grid gap-2">
              <Label>형식</Label>
              <Select value={format} onValueChange={(v) => setFormat(v as "anthropic" | "openai" | "openai-responses")}>
                <SelectTrigger>
                  <SelectValue placeholder="형식 선택" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="anthropic">Anthropic</SelectItem>
                  <SelectItem value="openai">OpenAI (Chat Completions)</SelectItem>
                  <SelectItem value="openai-responses">OpenAI (Responses API)</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}

          <div className="grid gap-2">
            <Label htmlFor="p-model">모델</Label>
            <div className="flex gap-2">
              <Input
                id="p-model"
                className="font-mono"
                placeholder="claude-opus-4-8"
                value={model}
                onChange={(e) => setModel(e.target.value)}
              />
              {/* modal: 이 Popover의 내용은 <body>로 portal되어 Sheet의 스크롤 잠금 밖에 있다.
                  modal이 없으면 목록은 그려지지만 스크롤되지 않는다. modal이면 Popover가 가장 위의 스크롤 잠금을 직접 갖는다. */}
              <Popover open={modelsOpen} onOpenChange={setModelsOpen} modal>
                <PopoverTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    size="icon"
                    className="shrink-0"
                    disabled={loadingModels}
                    onClick={loadModels}
                    title="API에서 쓸 수 있는 모델 불러오기"
                  >
                    {loadingModels ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
                  </Button>
                </PopoverTrigger>
                {models.length > 0 && (
                  <PopoverContent className="max-h-72 w-72 gap-0 overflow-y-auto overscroll-contain p-1" align="end">
                    {models.map((m) => (
                      <button
                        key={m}
                        type="button"
                        className="w-full shrink-0 rounded-md px-2 py-1.5 text-left font-mono text-xs hover:bg-accent hover:text-accent-foreground"
                        onClick={() => {
                          setModel(m);
                          setModelsOpen(false);
                        }}
                      >
                        {m}
                      </button>
                    ))}
                  </PopoverContent>
                )}
              </Popover>
            </div>
          </div>

          {!isOAuth && (
            <div className="grid gap-2">
              <Label htmlFor="p-base-url">Base URL(선택)</Label>
              <Input
                id="p-base-url"
                className="font-mono"
                placeholder="https://api.openai.com/v1"
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
              />
            </div>
          )}

          <div className="grid gap-2">
            <Label htmlFor="p-proxy">프록시(선택)</Label>
            <Input
              id="p-proxy"
              className="font-mono"
              placeholder="socks5://user:pass@127.0.0.1:1080 · http://127.0.0.1:8080"
              value={proxy}
              onChange={(e) => setProxy(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              LLM으로 나가는 요청만 이 프록시를 거칩니다. http/https/socks5를 지원하고 계정·비밀번호를 붙일 수
              있습니다(예: socks5://user:pass@host:port, 비밀번호에 특수 문자가 있으면 URL 인코딩하세요). 비워 두면
              프록시 없이 직접 연결합니다.
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-session-header">사용자 지정 세션 헤더(선택)</Label>
            <Input
              id="p-session-header"
              className="font-mono"
              placeholder="예: x-session-id(비워 두면 보내지 않음)"
              value={sessionHeaderKey}
              onChange={(e) => setSessionHeaderKey(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              헤더 이름을 넣으면 요청마다 이 HTTP 헤더를 붙이고, 값은 <b>현재 세션의 session id</b>로 자동으로
              채웁니다(chat 세션은 conv-12, worker는 exp3-worker-i87 같은 값). session-id 헤더로 프롬프트 캐시나 고정
              라우팅(sticky routing)을 하는 게이트웨이에 씁니다. 같은 세션에서는 여러 번 호출해도 값이 같고, 세션마다
              다릅니다. 비워 두면 보내지 않습니다.
            </p>
          </div>

          {!isOAuth && (
            <div className="grid gap-2">
              <Label htmlFor="p-api-key">API Key</Label>
              <Input
                id="p-api-key"
                type="password"
                placeholder={keyHint ? `설정됨(${keyHint}), 비워 두면 바꾸지 않음` : "sk-…"}
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
              />
            </div>
          )}

          <div className="grid gap-4 sm:grid-cols-3">
            <div className="grid gap-2">
              <Label htmlFor="p-rps">초당 속도 제한</Label>
              <Input id="p-rps" type="number" min={0} value={rps} onChange={(e) => setRps(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-rpm">분당 속도 제한</Label>
              <Input id="p-rpm" type="number" min={0} value={rpm} onChange={(e) => setRpm(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-cw">컨텍스트 창(K)</Label>
              <Input
                id="p-cw"
                type="number"
                min={0}
                max={1000}
                value={cw}
                onChange={(e) => setCw(e.target.value)}
                placeholder="200"
              />
            </div>
          </div>
          <p className="-mt-2 text-muted-foreground text-xs">
            속도 제한 0 = 제한 없음이며 모든 에이전트가 함께 씁니다. 컨텍스트 창 단위는 K(1,000토큰)이고 0 = 기본값
            200K, 최대 1000(즉 1M)입니다. 너무 크게 잡으면 압축이 실행되지 않습니다.
          </p>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-priority" className="text-sm">
                  장애 조치 우선순위
                </Label>
                <p className="text-muted-foreground text-xs">
                  숫자가 클수록 먼저 고릅니다. 활성 프로필은 이 값과 상관없이 항상 1순위입니다. 우선순위가 같은 프로필은
                  번갈아 앞에 서므로 사용 한도가 자연스럽게 나뉩니다.
                </p>
              </div>
              <Input
                id="p-priority"
                type="number"
                className="w-24 shrink-0"
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">장애 조치 제외</Label>
                <p className="text-muted-foreground text-xs">
                  켜면 장애 조치 대상으로 쓰지 않습니다(에이전트 / 작업이 직접 지정하면 여전히 씁니다). 「특정 에이전트
                  전용이라 다른 프로필이 실패했을 때 쓰이면 안 되는」 비싼 프로필에 알맞습니다.
                </p>
              </div>
              <Switch checked={poolExclude} onCheckedChange={setPoolExclude} aria-label="장애 조치 제외" />
            </div>
            <div className={cn("flex items-center justify-between gap-4 border-t pt-3", isChatGPT && "hidden")}>
              <div className="grid gap-0.5">
                <Label className="text-sm">스트리밍 출력 · streaming</Label>
                <p className="text-muted-foreground text-xs">
                  켜면(기본값) SSE 스트리밍을 써서 실행 중 진행 상황과 토큰 수를 실시간으로 봅니다. 끄면 실제
                  비스트리밍(stream:false, 완성된 응답을 한 번에 받음)을 써서 일부 게이트웨이의 불안정한 SSE 구현(빈
                  프레임 / 사고 필드 프레임 누락)을 피할 수 있지만, 실행 중 실시간 진행 상황은 볼 수 없습니다.
                </p>
              </div>
              <Switch checked={streaming} onCheckedChange={setStreaming} aria-label="스트리밍 출력" />
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-max-tokens" className="text-sm">
                  출력 최대 토큰 · max tokens
                </Label>
                <p className="text-muted-foreground text-xs">
                  응답 한 번에 만들 최대 토큰 수이며 요청마다 함께 보냅니다. 0(기본값) = 이 필드를 보내지 않고 서버
                  기본값을 따릅니다. 위의 「컨텍스트 창」과는 다릅니다. 그것은 모델의 전체 용량이고 압축 임계값을 계산할
                  때 로컬에서만 씁니다. 너무 작게 잡으면 추론 모델이 사고 단계에서 잘려 답을 한 글자도 내지 못합니다.
                </p>
              </div>
              <Input
                id="p-max-tokens"
                type="number"
                min={0}
                className="w-28 shrink-0"
                value={maxTokens}
                onChange={(e) => setMaxTokens(e.target.value)}
                placeholder="0"
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">최대 토큰 필드 이름</Label>
                <p className="text-muted-foreground text-xs">{MAX_TOKENS_FIELD_HINTS[effectiveFormat]}</p>
              </div>
              <Select
                value={effectiveFormat === "openai" ? maxTokensField : NONE}
                onValueChange={setMaxTokensField}
                disabled={effectiveFormat !== "openai"}
              >
                <SelectTrigger className="w-56 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {MAX_TOKENS_FIELDS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label className="text-sm">사고 스위치 · thinking.type</Label>
                <p className="text-muted-foreground text-xs">
                  thinking 필드를 보낼지 정합니다. 보내지 않음 = 필드를 넣지 않음(MiniMax처럼 지원하지 않는 모델과
                  호환), 끄기 = disabled를 보냄, 켜기 = enabled를 보냄. 아래 강도와는 서로 독립입니다.
                </p>
              </div>
              <Select value={thinkingType} onValueChange={setThinkingType}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {THINKING_TYPES.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">사고 강도 · reasoning_effort</Label>
                <p className="text-muted-foreground text-xs">
                  독립된 강도 단계입니다(OpenAI reasoning_effort / Anthropic output_config.effort). thinking 필드 없이
                  강도만으로 사고를 켜는 API가 있어서, 사고 스위치를 보내지 않고 이것만 따로 설정할 수 있습니다.
                </p>
              </div>
              <Select value={effort} onValueChange={setEffort}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {EFFORT_LEVELS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          {isClaude ? (
            <p className="text-muted-foreground text-xs">
              Claude 구독은 인증이 거절된 경우에만 토큰을 갱신하고 한 번 다시 요청합니다. 사용량 제한·서버 오류는
              자동으로 재시도하지 않습니다.
            </p>
          ) : (
            <ProfileRetryFields value={retry} onChange={setRetry} />
          )}
        </div>

        <div className="flex gap-2 border-t px-4 py-3">
          <Button variant="outline" onClick={testConnection} disabled={testing}>
            {testing ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}
            {testing ? "테스트 중…" : "연결 테스트"}
          </Button>
          <Button onClick={save} disabled={saving} className="flex-1">
            {saving && <Loader2Icon className="animate-spin" />}
            {!saving && (isNew ? <PlusIcon /> : <SaveIcon />)}
            {isNew ? "만들기" : "저장"}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────

export default function LLMPage() {
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [pool, setPool] = React.useState<LLMPoolStatus | null>(null);
  const [poolOpen, setPoolOpen] = React.useState(false);
  // 패널의 열림 상태와 내용을 따로 둔다. 닫을 때 editing을 그대로 두지 않으면 닫는 애니메이션 동안 제목이
  // 「편집 X」에서 「새로 만들기」로 깜박인다. editing = null은 새로 만들기다.
  const [editOpen, setEditOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<LLMProfile | null>(null);
  const openEditor = React.useCallback((p: LLMProfile | null) => {
    setEditing(p);
    setEditOpen(true);
  }, []);

  const loadPool = React.useCallback(async () => {
    try {
      setPool(await api.llmPool());
    } catch {
      /* ignore */
    }
  }, []);

  const load = React.useCallback(async () => {
    let loaded: LLMProfile[] = [];
    try {
      loaded = await api.llmProfiles();
      setProfiles(loaded);
    } catch {
      /* ignore */
    }
    await loadPool();
    return loaded;
  }, [loadPool]);

  // 구독 방식으로 막 저장한 프로필은 저장된 상태로 다시 열어야 로그인 버튼이 생긴다.
  const handleSaved = React.useCallback(
    (id: string, needsLogin: boolean) => {
      void load().then((loaded) => {
        const saved = needsLogin ? loaded.find((p) => p.id === id) : undefined;
        if (saved) openEditor(saved);
      });
    },
    [load, openEditor],
  );

  React.useEffect(() => {
    void load();
  }, [load]);

  // 카드의 상태 배지는 profile id로 장애 조치 상태를 가져온다.
  const health = React.useMemo(() => {
    const m = new Map<string, LLMPoolMember>();
    for (const c of pool?.chain ?? []) m.set(c.profile_id, c);
    return m;
  }, [pool]);

  async function activate(id: string, name: string) {
    try {
      await api.activateLLMProfile(id);
      toast.success(`활성으로 설정했습니다: ${name}`);
      await load();
    } catch (e) {
      toast.error(`활성으로 설정하지 못했습니다: ${(e as Error).message}`);
    }
  }

  async function remove(p: LLMProfile) {
    if (p.is_default) {
      toast.error("활성 프로필은 삭제할 수 없습니다");
      return;
    }
    try {
      await api.deleteLLMProfile(p.id);
      toast.success(`삭제했습니다: ${p.name}`);
      await load();
    } catch (e) {
      toast.error(`삭제하지 못했습니다: ${(e as Error).message}`);
    }
  }

  const poolOn = pool?.enabled ?? false;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">LLM</h1>
          <p className="text-muted-foreground text-sm">
            모든 에이전트가 함께 쓰는 형식 / 모델 / 속도 제한 설정입니다. 카드를 눌러 편집하고, 별표가 현재 활성
            프로필입니다.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => setPoolOpen(true)}>
            <ZapIcon /> 장애 조치 설정
            {poolOn && (
              <Badge variant="outline" className="ml-1 border-emerald-500/50 text-emerald-600 dark:text-emerald-400">
                켜짐
              </Badge>
            )}
          </Button>
          <Button size="sm" variant="outline" onClick={() => openEditor(null)}>
            <PlusIcon /> 새로 만들기
          </Button>
        </div>
      </div>

      <Tabs defaultValue="profiles" className="flex-1">
        <TabsList>
          <TabsTrigger value="profiles">LLM 프로필</TabsTrigger>
          <TabsTrigger value="retry">재시도와 백오프</TabsTrigger>
        </TabsList>

        <TabsContent value="profiles" className="mt-4">
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {profiles.map((p) => {
              const h = healthOf(p, health.get(p.id));
              return (
                // biome-ignore lint/a11y/useSemanticElements: 카드 안에 자체 버튼이 있어 기본 <button>을 쓰면 버튼이 중첩된다(잘못된 HTML)
                <Card
                  key={p.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => openEditor(p)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      openEditor(p);
                    }
                  }}
                  className={cn(
                    "cursor-pointer gap-0 py-4 outline-none transition-colors hover:border-foreground/30",
                    p.is_default && "border-amber-400/50 bg-amber-400/5",
                  )}
                >
                  <CardContent className="grid gap-2 px-4">
                    <div className="flex items-start gap-2">
                      <StarIcon
                        className={cn(
                          "mt-0.5 size-4 shrink-0",
                          p.is_default ? "fill-amber-400 text-amber-400" : "text-muted-foreground",
                        )}
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-medium text-sm">{p.name}</span>
                          <Badge variant="outline" className="uppercase">
                            {p.format}
                          </Badge>
                          <Badge variant="outline" className={cn("ml-auto", h.cls)} title={h.hint}>
                            {h.label}
                          </Badge>
                        </div>
                        <code className="mt-1 block truncate font-mono text-muted-foreground text-xs">{p.model}</code>
                      </div>
                    </div>

                    <div className="flex flex-wrap gap-x-3 gap-y-0.5 pl-6 text-muted-foreground text-xs">
                      {p.api_key_hint && <span>{p.api_key_hint}</span>}
                      {p.oauth?.plan && (
                        <span>
                          {p.auth_type === "claude_oauth" ? "Claude" : "ChatGPT"} {p.oauth.plan}
                        </span>
                      )}
                      <span>
                        {p.rate_per_second}/s · {p.rate_per_minute}/min
                      </span>
                      {p.proxy && <span className="truncate">프록시 {p.proxy}</span>}
                      {p.reasoning_effort && (
                        <span>사고 {p.reasoning_effort === "off" ? "끔" : p.reasoning_effort}</span>
                      )}
                      {/* 장애 조치 관련 두 필드는 장애 조치가 켜져 있을 때만 의미가 있으므로, 꺼져 있으면 자리를 차지하지 않는다 */}
                      {poolOn &&
                        !p.is_default &&
                        (p.pool_exclude ? <span>장애 조치 제외</span> : <span>우선순위 {p.priority ?? 0}</span>)}
                    </div>

                    <div className="mt-1 flex gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        className="flex-1"
                        disabled={p.is_default}
                        onClick={(e) => {
                          e.stopPropagation();
                          void activate(p.id, p.name);
                        }}
                      >
                        {p.is_default ? "활성" : "활성으로 설정"}
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="프로필 삭제"
                        onClick={(e) => {
                          e.stopPropagation();
                          void remove(p);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              );
            })}
            {profiles.length === 0 && (
              <div className="col-span-full rounded-lg border border-dashed p-10 text-center text-muted-foreground text-sm">
                LLM 프로필이 아직 없습니다. 오른쪽 위의 「새로 만들기」를 눌러 첫 프로필을 만드세요.
              </div>
            )}
          </div>
        </TabsContent>

        <TabsContent value="retry" className="mt-4">
          <RetryPolicyPanel />
        </TabsContent>
      </Tabs>

      <ProfileSheet profile={editing} open={editOpen} onOpenChange={setEditOpen} onSaved={handleSaved} />
      <PoolSheet open={poolOpen} onOpenChange={setPoolOpen} pool={pool} onReload={loadPool} />
    </div>
  );
}
