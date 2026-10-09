import type { NewAssetType } from "@/lib/types";

const ASSET_TYPE_LABELS: Record<NewAssetType, string> = {
  app: "애플리케이션",
  endpoint: "엔드포인트",
  ip: "IP",
  root_domain: "루트 도메인",
  service: "서비스",
  subdomain: "하위 도메인",
};

const TASK_ASSET_SOURCE_LABELS: Record<string, string> = {
  agent: "에이전트 발견",
  anchor: "블랙보드 앵커",
  api: "자산 API",
  company: "기업 관련",
  legacy: "과거 관련",
  manual: "수동 추가",
  system: "시스템 관련",
  task: "작업 초기화",
};

export function taskAssetTypeLabel(type: NewAssetType): string {
  return ASSET_TYPE_LABELS[type];
}

export function taskAssetSourceLabel(source: string): string {
  return TASK_ASSET_SOURCE_LABELS[source] ?? source;
}
