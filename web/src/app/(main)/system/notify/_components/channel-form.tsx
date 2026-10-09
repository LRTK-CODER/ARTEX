"use client";

import { CheckIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { NotificationFilter } from "@/lib/types";

// asText / inputType은 이 파일 안에서만 쓰는 값 도우미다(컨트롤 렌더링과 밀접해서 channel-fields에 두지 않는다).
import { type FieldDef, type FieldKind, SEVERITY_OPTIONS } from "./channel-fields";

// asText는 임의의 설정값을 입력 칸에 쓸 수 있는 문자열로 바꾼다.
// config는 JSON에서 오므로 값이 string / number / boolean / array / null일 수 있다.
// 여기서는 「텍스트 칸에 넣을 수 있는가」만 본다. 실제 직렬화는 buildConfig가 맡는다.
function asText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  return String(v);
}

// inputType은 필드 유형을 input의 type 속성으로 바꾼다.
function inputType(kind: FieldKind): "text" | "password" | "number" {
  if (kind === "password") return "password";
  if (kind === "number") return "number";
  return "text";
}

// ConfigField는 필드 정의에 맞는 컨트롤을 그린다.
//
// 여기서 신경 쓸 것은 마스킹된 필드 처리 하나뿐이다. 입력 칸에 마스킹 값 자체를 **보여 주지 않고**
// 「저장됨」 안내 한 줄만 보여 준다. 그러면 화면의 규칙이 하나가 된다. 칸에 글자가 있으면 사용자가 입력한 것이고,
// 빈 칸은 빈 값이다. "__masked__:…abc123"을 입력 칸에 넣으면 사용자는 직접 지워야 하는
// 자리표시 텍스트로 여겨 오히려 자격 증명을 실수로 지우기 쉽다.
export function ConfigField({
  def,
  value,
  isSecret,
  onChange,
}: {
  def: FieldDef;
  value: unknown;
  isSecret: boolean;
  onChange: (v: unknown) => void;
}) {
  const id = `n-cfg-${def.key}`;
  const raw = asText(value);
  // 백엔드가 돌려준 마스킹 값: "__masked__:…abc123" 형태이고, 끝부분은 원래 값을 알아볼 수 있는 조각이다.
  const masked = isSecret && raw.startsWith("__masked__");
  const maskedTail = masked ? (raw.split("…")[1] ?? "") : "";

  if (def.kind === "switch") {
    return (
      <div className="flex items-center gap-2 text-sm">
        <Switch checked={value === true} onCheckedChange={onChange} aria-label={def.label} />
        {def.label}
        {def.help && <span className="text-muted-foreground">（{def.help}）</span>}
      </div>
    );
  }

  if (def.kind === "select") {
    return (
      <div className="grid gap-2">
        <Label>{def.label}</Label>
        <Select value={raw || def.options?.[0]?.value} onValueChange={onChange}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(def.options ?? []).map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  // 필드 유형에 따라 컨트롤을 고른다. 네 가지 컨트롤을 구분해야 해서 중첩 삼항 대신 if 사슬을 쓴다.
  // 삼항이 세 겹이면 읽다가 괄호를 세어야 한다.
  function control() {
    if (def.kind === "textarea" || def.kind === "kv") {
      return (
        <Textarea
          id={id}
          className="font-mono"
          placeholder={def.placeholder}
          value={masked ? "" : raw}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    }
    if (def.kind === "list") {
      return (
        <Input
          id={id}
          value={Array.isArray(value) ? (value as string[]).join(", ") : raw}
          onChange={(e) => onChange(e.target.value)}
          placeholder={def.placeholder}
        />
      );
    }
    return (
      <Input
        id={id}
        className={def.kind === "text" ? "font-mono" : ""}
        type={inputType(def.kind)}
        placeholder={def.placeholder}
        value={masked ? "" : raw}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  const hint = masked ? (
    <p className="text-muted-foreground flex items-center gap-1 text-xs">
      <CheckIcon className="size-3" />
      저장됨{maskedTail ? `(끝자리 ${maskedTail})` : ""} · 새 값을 입력하면 덮어쓰고, 비우면 이 항목을 삭제합니다
    </p>
  ) : (
    def.help && <p className="text-muted-foreground text-xs">{def.help}</p>
  );

  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{def.label}</Label>
      {control()}
      {hint}
    </div>
  );
}

// FilterSummary는 필터 조건을 한 줄로 요약해, 카드를 펼치지 않아도 이 채널이 무엇을 보내는지 알 수 있게 한다.
export function FilterSummary({ filter }: { filter: NotificationFilter }) {
  const parts: string[] = [];
  if (filter.min_severity) {
    parts.push(SEVERITY_OPTIONS.find((o) => o.value === filter.min_severity)?.label ?? filter.min_severity);
  }
  if (filter.vulnclass_include?.length) parts.push(`포함 유형 키워드 ${filter.vulnclass_include.length}개`);
  if (filter.vulnclass_exclude?.length) parts.push(`제외 키워드 ${filter.vulnclass_exclude.length}개`);
  if (filter.task_ids?.length) parts.push(`작업 ${filter.task_ids.length}개`);
  if (filter.asset_ids?.length) parts.push(`자산 ${filter.asset_ids.length}개`);
  if (filter.on_status_change) parts.push("상태 변경 포함");
  if (parts.length === 0) {
    return <p className="text-muted-foreground text-sm">모든 취약점</p>;
  }
  return <p className="text-muted-foreground text-sm">{parts.join(" · ")}</p>;
}
