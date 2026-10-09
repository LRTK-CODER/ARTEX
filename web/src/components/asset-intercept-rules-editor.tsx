"use client";

import * as React from "react";

import { PlusIcon, Trash2Icon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import type { AssetInterceptKind, AssetInterceptRuleInput } from "@/lib/types";

// shadcn Select 대신 NativeSelect(브라우저 기본 <select>)를 쓴다. 이 편집기는 Sheet 서랍 안에서 쓰이는데,
// shadcn Select의 드롭다운은 body로 portal되어 바깥 클릭으로 처리되고, 서랍의 '바깥 클릭 시 닫기'가 잘못 동작한다. 기본 드롭다운에는 이 문제가 없다.
export const ASSET_INTERCEPT_KIND_OPTIONS: {
  value: AssetInterceptKind;
  label: string;
  placeholder: string;
}[] = [
  { value: "exact_domain", label: "도메인(정확히 일치)", placeholder: "example.gov.cn" },
  { value: "exact_ip", label: "IP(정확히 일치)", placeholder: "203.0.113.10" },
  { value: "exact_url", label: "URL(정확히 일치)", placeholder: "https://example.com/login" },
  { value: "fuzzy_domain", label: "도메인(부분 일치)", placeholder: ".gov.cn" },
  { value: "fuzzy_ip", label: "IP(부분 일치)", placeholder: "203.0.113." },
  { value: "fuzzy_url", label: "URL(부분 일치)", placeholder: "/admin" },
  { value: "cidr", label: "CIDR 대역", placeholder: "192.168.0.0/16" },
];

// AssetInterceptRulesEditor는 '차단/허용 규칙'을 여러 행으로 편집하는 제어 컴포넌트다(차단 block/허용 allow +
// 유형 + 대조할 값 + 메모). 저장은 직접 하지 않고, 언제 제출할지는 부모 컴포넌트가 정한다.
export function AssetInterceptRulesEditor({
  value,
  onChange,
}: {
  value: AssetInterceptRuleInput[];
  onChange: (v: AssetInterceptRuleInput[]) => void;
}) {
  function update(i: number, patch: Partial<AssetInterceptRuleInput>) {
    onChange(value.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
  }
  function remove(i: number) {
    onChange(value.filter((_, idx) => idx !== i));
  }
  function add() {
    onChange([...value, { action: "block", kind: "fuzzy_domain", pattern: "", note: "", enabled: true }]);
  }
  return (
    <div className="grid gap-2">
      {value.map((r, i) => {
        const ph = ASSET_INTERCEPT_KIND_OPTIONS.find((o) => o.value === r.kind)?.placeholder ?? "";
        return (
          // biome-ignore lint/suspicious/noArrayIndexKey: 행에 안정적인 id가 없어 인덱스로 제어하면 된다
          <div key={i} className="flex items-center gap-2">
            <NativeSelect
              size="sm"
              className="w-[84px] shrink-0"
              value={r.action}
              onChange={(e) => update(i, { action: e.target.value as "block" | "allow" })}
            >
              <NativeSelectOption value="block">차단</NativeSelectOption>
              <NativeSelectOption value="allow">허용</NativeSelectOption>
            </NativeSelect>
            <NativeSelect
              size="sm"
              className="w-[120px] shrink-0"
              value={r.kind}
              onChange={(e) => update(i, { kind: e.target.value as AssetInterceptKind })}
            >
              {ASSET_INTERCEPT_KIND_OPTIONS.map((o) => (
                <NativeSelectOption key={o.value} value={o.value}>
                  {o.label}
                </NativeSelectOption>
              ))}
            </NativeSelect>
            <Input
              className="flex-1"
              placeholder={ph}
              value={r.pattern}
              onChange={(e) => update(i, { pattern: e.target.value })}
            />
            <Input
              className="w-[120px] shrink-0"
              placeholder="메모(선택)"
              value={r.note}
              onChange={(e) => update(i, { note: e.target.value })}
            />
            <Button
              type="button"
              size="icon"
              variant="ghost"
              className="text-destructive hover:text-destructive size-8 shrink-0"
              onClick={() => remove(i)}
            >
              <Trash2Icon className="size-4" />
            </Button>
          </div>
        );
      })}
      <Button type="button" size="sm" variant="outline" className="w-fit" onClick={add}>
        <PlusIcon className="size-4" /> 규칙 추가
      </Button>
    </div>
  );
}
