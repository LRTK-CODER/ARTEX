"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 새 버전이 뜨기를 기다리는 최대 시간. 업데이트 한 번에 프로세스가 세 번 시작된다(준비 → 교체 → 새 버전).
 *  각각 몇 초면 끝나므로 3분이면 느린 디스크와 Docker 컨테이너 재생성까지 충분히 덮는다. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // progress와 따로 둔다. 준비가 끝나면 프로세스가 사라져 SSE가 끊기므로, 그때는 /api/health 폴링으로 바꿔야 한다.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet는 백엔드 캐시를 건너뛸지도 정한다. 페이지에 들어올 때의 자동 확인은 캐시를 쓰고(상단 바가 방금 확인했다),
  // 사용자가 '업데이트 확인'을 직접 누르면 원본을 다시 조회한다. 그러지 않으면 방금 나온 버전이 캐시가 만료돼야 보인다.
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("업데이트를 확인하지 못했습니다: " + r.error);
          else if (r.has_update) toast.success(`새 버전 ${r.latest}이(가) 있습니다`);
          else if (r.comparable) toast.success("이미 최신 버전입니다");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("업데이트를 확인하지 못했습니다: " + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // 버전 번호가 바뀔 때까지 /api/health를 폴링한다.
  //
  // 판정 기준은 "연결된다"가 아니라 "버전이 바뀌었다"여야 한다. 교체 중에 이전 버전이 잠깐 다시 뜬다
  // (그때는 artex.new로 바꿔 넣고 바로 종료할 뿐이다). 연결만 보면 성공으로 잘못 판정한다.
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`${j.version}(으)로 업데이트했습니다. 페이지를 다시 불러오는 중입니다.`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 재시작하는 동안 연결되지 않는 것은 예상한 일이므로 폴링을 계속한다.
      }
    }
    setRestarting(false);
    toast.error(
      "서비스 재시작을 기다리다 시간이 초과됐습니다. 백엔드 로그를 확인하거나, artex를 start.sh / start.bat으로 시작했는지 확인하세요.",
    );
  }, []);

  // 업데이트 진행 상황을 구독한다. SSE는 Next의 /api 리라이트를 거치지 않는다(그 계층이 버퍼링해 이벤트가 전달되지 않는다).
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("업데이트를 설치하지 못했습니다: " + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // 프로세스가 끝나면 SSE는 반드시 끊긴다. 이미 재시작 대기에 들어갔다면 정상이므로
        // /api/health 폴링이 이어서 판정하게 둔다.
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `${info.latest}(으)로 업데이트할까요?\n\n` +
        "업데이트하면 프로그램이 재시작되고 실행 중인 작업이 중단됩니다.\n" +
        (info.mode === "docker"
          ? "\n주의: 컨테이너 안의 업데이트는 프로그램만 바꾸고 이미지의 playwright / nmap 등 도구 모음은 업데이트하지 않습니다. " +
            "새 버전에 새 도구가 필요하면 docker compose pull을 쓰세요."
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "준비 중…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("업데이트를 시작하지 못했습니다: " + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "이전 버전으로 롤백할까요?\n\n프로그램이 재시작되고 실행 중인 작업이 중단됩니다.\n주의: 데이터베이스 구조는 되돌리지 않으므로 이전 버전이 새 버전이 쓴 데이터를 읽지 못할 수 있습니다.",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("이전 버전으로 바꿨습니다. 재시작하는 중…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("롤백하지 못했습니다: " + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 실제 백분율은 다운로드 단계에서만 얻는다(Content-Length로 계산). 검증·압축 해제·재시작 대기는
  // 걸리는 시간을 알 수 없는 단계라, 진행 막대를 채우고 깜박임 애니메이션으로 "작업 중이지만 얼마나 걸릴지 모름"을 나타낸다.
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 설정 페이지는 다단 메이슨리 레이아웃이라 카드가 직접 행 간격을 주고 단 사이에서 끊기지 않게 한다(page.tsx 주석 참고).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          버전과 업데이트
        </CardTitle>
        <CardDescription>
          GitHub에서 새 버전을 확인하고 설치합니다. 업데이트하면 프로그램이 재시작되고 실행 중인 작업이 중단됩니다.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">현재 버전</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "단독 실행 파일"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">최신 버전</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              변경 내역 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            GitHub에 연결하지 못했습니다: {info.error} 위에서 전역 프록시를 설정한 뒤 다시 시도하세요.
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest}에 {info.os}/{info.arch}용 릴리스 파일이 없어({info.asset} 없음) 자동으로 업데이트할 수
            없습니다.
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            <span className="font-mono">{info.asset}</span> {info.size ? `(${humanSize(info.size)})` : ""}을(를)
            내려받아 SHA256 검증과 스모크 테스트를 통과해야 교체합니다. 실패하면 현재 버전을 자동으로 유지합니다.
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            이미 최신 버전입니다.
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker에서의 업데이트는 프로그램만 바꾸고 이미지의 playwright / nmap 등 도구 모음은 업데이트하지 않습니다.
            또 <span className="font-mono"> docker compose up -d </span>로 컨테이너를 다시 만들면 이미지에 든 버전으로
            돌아갑니다. 이미지까지 함께 올리려면 다음을 실행하세요:{" "}
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting
                ? "재시작하며 새 버전을 적용하는 중입니다. 잠시 기다리세요(페이지가 자동으로 새로 고쳐집니다)…"
                : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            업데이트 확인
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `${info.latest}(으)로 업데이트` : "지금 업데이트"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              이전 버전으로 롤백
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          원클릭 업데이트는 감시 스크립트가 프로그램을 재시작하는 것에 기댑니다.{" "}
          <span className="font-mono">start.sh</span>(Windows는<span className="font-mono"> start.bat</span>)로 ARTEX를
          시작하세요. artex 실행 파일을 직접 실행하면 프로그램이 끝난 뒤 자동으로 다시 시작되지 않습니다.
        </p>
      </CardContent>
    </Card>
  );
}
