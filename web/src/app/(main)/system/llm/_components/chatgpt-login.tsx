"use client";

import * as React from "react";

import { ExternalLinkIcon, Loader2Icon, LogInIcon, UnplugIcon } from "lucide-react";
import { toast } from "sonner";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import {
  DEVICE_POLL_INTERVAL_MS,
  deviceFailureMessage,
  errorCode,
  loginErrorMessage,
  type OAuthConnection,
  oauthConnection,
  shouldRetryPoll,
} from "@/lib/chatgpt-oauth";
import type { ChatGPTDeviceStart, ChatGPTLoginStart, LLMProfile, LLMProfileOAuth } from "@/lib/types";

// access·refresh 토큰은 서버에만 있다. 화면은 flow_id와 연결 상태를 들고, 붙여넣은 콜백 주소도
// 보낸 즉시 지운다. 그래서 이 파일은 브라우저 저장소·URL·콘솔에 아무것도 남기지 않는다.

const CONNECTION_BADGES: Record<OAuthConnection, { label: string; cls: string }> = {
  connected: { label: "연결됨", cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400" },
  disconnected: { label: "연결 안 됨", cls: "border-muted-foreground/40 text-muted-foreground" },
  needs_login: { label: "재로그인 필요", cls: "border-amber-500/50 text-amber-600 dark:text-amber-400" },
};

function ErrorText({ message }: { message: string }) {
  if (!message) return null;
  return (
    <p role="alert" className="text-destructive text-xs">
      {message}
    </p>
  );
}

// DeviceLogin 은 디바이스 코드를 받아 보여 주고, 끝날 때까지 상태를 폴링한다.
// 탭을 바꿔도 마운트된 채 폴링을 이어 간다. 대화 상자가 닫히면 언마운트되며 폴링도 멈춘다.
// 만료는 브라우저 시계로 판정하지 않고 서버의 expired 상태를 따른다.
function DeviceLogin({ profileId, onConnected }: { profileId: number; onConnected: () => void }) {
  const [device, setDevice] = React.useState<ChatGPTDeviceStart | null>(null);
  const [isStarting, setIsStarting] = React.useState(false);
  const [error, setError] = React.useState("");

  React.useEffect(() => {
    if (!device) return;
    let isStopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let failedAttempts = 0;
    const stopWith = (message: string) => {
      setError(message);
      setDevice(null);
    };
    const poll = async () => {
      try {
        const r = await api.chatGPTDeviceStatus(device.flow_id);
        if (isStopped) return;
        failedAttempts = 0;
        if (r.status === "succeeded") {
          onConnected();
          return;
        }
        if (r.status !== "pending") {
          stopWith(deviceFailureMessage(r.status, r.code));
          return;
        }
      } catch (e) {
        if (isStopped) return;
        failedAttempts++;
        if (!shouldRetryPoll(e, failedAttempts)) {
          stopWith(loginErrorMessage(errorCode(e)));
          return;
        }
      }
      timer = setTimeout(() => void poll(), DEVICE_POLL_INTERVAL_MS);
    };
    timer = setTimeout(() => void poll(), DEVICE_POLL_INTERVAL_MS);
    return () => {
      isStopped = true;
      clearTimeout(timer);
    };
  }, [device, onConnected]);

  async function start() {
    setIsStarting(true);
    setError("");
    try {
      setDevice(await api.startChatGPTDeviceLogin(profileId));
    } catch (e) {
      setError(loginErrorMessage(errorCode(e)));
    } finally {
      setIsStarting(false);
    }
  }

  return (
    <div className="grid gap-3 text-sm">
      <p className="text-muted-foreground text-xs">
        코드를 받은 뒤 확인 주소를 열어 ChatGPT에 로그인하고 코드를 입력하세요. 이 창은 승인될 때까지 기다립니다.
      </p>
      {device ? (
        <div className="grid gap-2 rounded-lg border p-3">
          <Label className="text-xs">사용자 코드</Label>
          <code className="select-all font-mono font-semibold text-2xl tracking-widest">{device.user_code}</code>
          <a
            href={device.verification_url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 break-all text-primary text-xs underline"
          >
            {device.verification_url} <ExternalLinkIcon className="size-3 shrink-0" />
          </a>
          <p className="flex items-center gap-2 text-muted-foreground text-xs">
            <Loader2Icon className="size-3 animate-spin" /> 승인을 기다리는 중…
          </p>
        </div>
      ) : (
        <Button onClick={() => void start()} disabled={isStarting}>
          {isStarting ? <Loader2Icon className="animate-spin" /> : <LogInIcon />}
          {error ? "코드 다시 받기" : "코드 받기"}
        </Button>
      )}
      <ErrorText message={error} />
    </div>
  );
}

// PasteLogin 은 로그인 주소를 새 탭으로 열게 하고, 로그인 뒤 브라우저가 연결하지 못한
// localhost 콜백 주소를 붙여넣어 받는다.
function PasteLogin({
  profileId,
  onConnected,
  provider = "chatgpt",
}: {
  profileId: number;
  onConnected: () => void;
  provider?: "chatgpt" | "claude";
}) {
  const label = provider === "claude" ? "Claude" : "ChatGPT";
  const callback = provider === "claude" ? "http://localhost:53692/callback" : "http://localhost:1455/auth/callback";
  const [flow, setFlow] = React.useState<ChatGPTLoginStart | null>(null);
  const [callbackUrl, setCallbackUrl] = React.useState("");
  const [isBusy, setIsBusy] = React.useState(false);
  const [error, setError] = React.useState("");

  async function start() {
    setIsBusy(true);
    setError("");
    try {
      setFlow(await (provider === "claude" ? api.startClaudeLogin(profileId) : api.startChatGPTLogin(profileId)));
    } catch (e) {
      setError(loginErrorMessage(errorCode(e), provider));
    } finally {
      setIsBusy(false);
    }
  }

  async function complete() {
    if (!flow) return;
    // 복사할 때 붙은 앞뒤 공백 때문에 callback_url_mismatch가 나지 않게 다듬는다.
    const url = callbackUrl.trim();
    if (!url) {
      setError("콜백 주소를 붙여넣으세요.");
      return;
    }
    // 주소에 일회용 code·state가 들어 있으므로 보내는 즉시 입력란에서 지운다.
    setCallbackUrl("");
    setIsBusy(true);
    setError("");
    try {
      if (provider === "claude") await api.completeClaudeLogin(profileId, flow.flow_id, url);
      else await api.completeChatGPTLogin(profileId, flow.flow_id, url);
      onConnected();
    } catch (e) {
      setError(loginErrorMessage(errorCode(e), provider));
    } finally {
      setIsBusy(false);
    }
  }

  return (
    <div className="grid gap-3 text-sm">
      <ol className="list-decimal space-y-1 pl-4 text-muted-foreground text-xs">
        <li>로그인 주소를 만들고 새 탭에서 열어 {label}에 로그인합니다.</li>
        <li>
          로그인이 끝나면 브라우저가 <code className="font-mono">{callback}?…</code> 로 이동하며 연결할 수 없다는 화면이
          뜹니다. 정상입니다.
        </li>
        <li>그 탭의 주소창 주소를 통째로 복사해 아래에 붙여넣습니다.</li>
        {provider === "claude" && (
          <li>
            주소 대신 <code>code#state</code> 전체를 붙여 넣어도 됩니다.
          </li>
        )}
      </ol>
      <Button variant={flow ? "outline" : "default"} onClick={() => void start()} disabled={isBusy}>
        {isBusy && !flow ? <Loader2Icon className="animate-spin" /> : <LogInIcon />}
        {flow ? "로그인 주소 다시 만들기" : "로그인 주소 만들기"}
      </Button>
      {flow && (
        <>
          <a
            href={flow.authorize_url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 text-primary text-xs underline"
          >
            새 탭에서 {label} 로그인 열기 <ExternalLinkIcon className="size-3" />
          </a>
          <code className="line-clamp-2 break-all font-mono text-[11px] text-muted-foreground">
            {flow.authorize_url}
          </code>
          <div className="grid gap-2">
            <Label htmlFor="subscription-callback-url">콜백 주소</Label>
            <Input
              id="subscription-callback-url"
              className="font-mono"
              autoComplete="off"
              spellCheck={false}
              placeholder={`${callback}?code=…&state=…`}
              value={callbackUrl}
              onChange={(e) => setCallbackUrl(e.target.value)}
            />
          </div>
          <Button onClick={() => void complete()} disabled={isBusy}>
            {isBusy && <Loader2Icon className="animate-spin" />} 연결하기
          </Button>
        </>
      )}
      <ErrorText message={error} />
    </div>
  );
}

// SubscriptionAccount 는 구독 프로필의 연결 상태를 보이고 제공자별 로그인·연결 해제를 연다.
// 로그인 API가 저장된 프로필 id를 받으므로, 아직 그 방식으로 저장되지 않았으면 안내만 한다.
export function SubscriptionAccount({
  profile,
  onChanged,
  provider = "chatgpt",
}: {
  profile: LLMProfile | null;
  onChanged: () => void;
  provider?: "chatgpt" | "claude";
}) {
  const label = provider === "claude" ? "Claude" : "ChatGPT";
  const [oauth, setOAuth] = React.useState<LLMProfileOAuth | undefined>(profile?.oauth);
  const [isLoginOpen, setIsLoginOpen] = React.useState(false);
  const [isConfirmOpen, setIsConfirmOpen] = React.useState(false);
  const [isDisconnecting, setIsDisconnecting] = React.useState(false);

  React.useEffect(() => setOAuth(profile?.oauth), [profile]);

  const profileId = profile ? Number(profile.id) : 0;
  const profileKey = profile?.id;

  // 로그인 응답에는 만료 시각이 없으므로 목록을 다시 읽어 상태를 맞춘다.
  const refresh = React.useCallback(async () => {
    try {
      const profiles = await api.llmProfiles();
      setOAuth(profiles.find((p) => p.id === profileKey)?.oauth);
    } catch (e) {
      toast.error(`연결 상태를 다시 읽지 못했습니다: ${(e as Error).message}`);
    }
    onChanged();
  }, [profileKey, onChanged]);

  const handleConnected = React.useCallback(() => {
    setIsLoginOpen(false);
    toast.success(`${label} 구독을 연결했습니다`);
    void refresh();
  }, [refresh, label]);

  if (profile?.auth_type !== `${provider}_oauth`) {
    return (
      <div className="rounded-lg border border-dashed p-3 text-muted-foreground text-xs">
        {label} 구독 프로필로 먼저 저장해야 로그인할 수 있습니다. 아래 버튼으로 저장하면 로그인 화면이 열립니다.
      </div>
    );
  }

  async function disconnect() {
    setIsDisconnecting(true);
    try {
      if (provider === "claude") await api.disconnectClaude(profileId);
      else await api.disconnectChatGPT(profileId);
      toast.success("연결을 해제했습니다");
    } catch (e) {
      toast.error(loginErrorMessage(errorCode(e), provider));
    } finally {
      setIsDisconnecting(false);
      setIsConfirmOpen(false);
    }
    await refresh();
  }

  const connection = oauthConnection(oauth);
  const badge = CONNECTION_BADGES[connection];
  return (
    <div className="grid gap-2 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <Label className="text-sm">{label} 구독</Label>
        <Badge variant="outline" className={badge.cls}>
          {badge.label}
        </Badge>
        {oauth?.plan && <Badge variant="outline">{oauth.plan}</Badge>}
      </div>
      {oauth?.connected && oauth.expires_at && (
        <p className="text-muted-foreground text-xs">
          토큰 만료 {new Date(oauth.expires_at).toLocaleString()} · 만료 전에 서버가 자동으로 갱신합니다.
        </p>
      )}
      {connection === "needs_login" && (
        <p className="text-amber-600 text-xs dark:text-amber-400">토큰 갱신이 거절됐습니다. 다시 로그인하세요.</p>
      )}
      <div className="flex gap-2">
        <Button size="sm" onClick={() => setIsLoginOpen(true)}>
          <LogInIcon /> {connection === "disconnected" ? "로그인" : "다시 로그인"}
        </Button>
        {oauth?.connected && (
          <Button size="sm" variant="outline" onClick={() => setIsConfirmOpen(true)}>
            <UnplugIcon /> 연결 해제
          </Button>
        )}
      </div>

      <Dialog open={isLoginOpen} onOpenChange={setIsLoginOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{label} 구독 로그인</DialogTitle>
            <DialogDescription>
              {provider === "claude"
                ? "새 탭에서 로그인하고 콜백 주소를 붙여 넣으세요."
                : "로그인 방식 하나를 고르세요. 창을 닫으면 진행 중인 대기를 멈춥니다."}
            </DialogDescription>
          </DialogHeader>
          {provider === "claude" ? (
            <PasteLogin profileId={profileId} onConnected={handleConnected} provider="claude" />
          ) : (
            <Tabs defaultValue="device">
              <TabsList className="w-full">
                <TabsTrigger value="device">디바이스 코드</TabsTrigger>
                <TabsTrigger value="paste">콜백 주소 붙여넣기</TabsTrigger>
              </TabsList>
              {/* forceMount: 탭을 오가도 받은 디바이스 코드·폴링과 붙여넣기 흐름을 잃지 않게 숨기기만 한다. */}
              <TabsContent value="device" forceMount className="mt-3 data-[state=inactive]:hidden">
                <DeviceLogin profileId={profileId} onConnected={handleConnected} />
              </TabsContent>
              <TabsContent value="paste" forceMount className="mt-3 data-[state=inactive]:hidden">
                <PasteLogin profileId={profileId} onConnected={handleConnected} />
              </TabsContent>
            </Tabs>
          )}
        </DialogContent>
      </Dialog>

      <AlertDialog open={isConfirmOpen} onOpenChange={setIsConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{label} 구독 연결을 해제할까요?</AlertDialogTitle>
            <AlertDialogDescription>
              저장된 자격 증명을 지웁니다. 이 프로필을 쓰는 Agent는 다시 로그인할 때까지 호출에 실패합니다.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={isDisconnecting}>취소</AlertDialogCancel>
            <AlertDialogAction
              disabled={isDisconnecting}
              onClick={(e) => {
                // 기본 동작은 바로 닫힌다. 요청이 끝난 뒤 닫아 진행 중 표시를 보이게 한다.
                e.preventDefault();
                void disconnect();
              }}
            >
              {isDisconnecting && <Loader2Icon className="animate-spin" />} 연결 해제
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
