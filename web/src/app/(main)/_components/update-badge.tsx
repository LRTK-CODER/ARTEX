"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * 상단 바의 '새 버전' 표시. 페이지 전체를 불러올 때 한 번 조회하고, 업데이트가 있으면 버전 번호 옆에 띄운다.
 * 누르면 시스템 설정 페이지의 '버전과 업데이트' 카드로 바로 간다.
 *
 * 백엔드가 GitHub 조회 결과를 30분 캐시하므로 여기서 마운트할 때마다 조회해도 안전하다.
 * 인증 없는 GitHub API는 IP당 시간당 60회뿐이라, 그 캐시가 없으면 탭 몇 개만 열어도
 * 사용 한도가 바닥나 정작 업데이트하려 할 때 조회하지 못한다.
 *
 * 조회에 실패하면 늘 조용히 넘긴다. 상단 바는 오류를 알릴 곳이 아니고, 사용자가 설정 페이지에서 '업데이트 확인'을 누르면 원인을 볼 수 있다.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update에 이미 '버전 번호를 비교할 수 있는가' 판단이 들어 있어 개발 빌드에서는 이 표시가 뜨지 않는다.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 조용히 넘긴다. 네트워크가 없거나 GitHub 속도 제한에 걸려도 상단 바에 오류를 띄우지 않는다.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`새 버전 ${latest}이(가) 있습니다. 눌러서 업데이트하세요`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 깜박이는 점: 상단 바에 요소가 많아 글자만으로는 놓치기 쉬우므로 움직임으로 바로 눈에 띄게 한다. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">새 버전 {latest}</span>
      <span className="sm:hidden">새 버전</span>
    </Link>
  );
}
