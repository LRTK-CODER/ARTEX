"use client";

import * as React from "react";

import { ExplorationGraph } from "@/components/exploration-graph";
import { api } from "@/lib/api";
import type { Edge, TaskNode } from "@/lib/types";

// FindingLineageView는 작업의 첫 노드에서 이 발견 사항의 노드까지 이어지는 탐색 하위 그래프를 그린다.
// 작업 그래프와 같은 공격 경로 그래프 캔버스를 쓰고, 범위만 이 발견 사항의 경로로 좁힌다.
export function FindingLineageView({ findingId }: { findingId: string }) {
  const [nodes, setNodes] = React.useState<TaskNode[]>([]);
  const [edges, setEdges] = React.useState<Edge[]>([]);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let alive = true;
    api
      .findingLineage(findingId)
      .then((g) => {
        if (!alive) return;
        setNodes(g.nodes ?? []);
        setEdges(g.edges ?? []);
      })
      .catch(() => {
        // 불러오지 못하면 이전 상태를 그대로 둔다(기존 동작). 오류 알림은 띄우지 않는다.
      })
      .finally(() => {
        if (alive) setLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, [findingId]);

  if (loaded && nodes.length === 0) {
    return (
      <p className="text-muted-foreground p-6 text-sm">
        표시할 공격 경로가 없습니다(이 취약점과 관련된 탐색 노드가 없거나 소속 작업이 삭제됐습니다).
      </p>
    );
  }

  return (
    <ExplorationGraph
      nodes={nodes}
      edges={edges}
      className="h-[68vh]"
      emptyHint={loaded ? "공격 경로 없음" : "불러오는 중…"}
    />
  );
}
