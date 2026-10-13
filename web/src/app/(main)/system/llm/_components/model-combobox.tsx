"use client";

import * as React from "react";

import { Loader2Icon, PencilIcon, RefreshCwIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Combobox,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
} from "@/components/ui/combobox";

// isCustom: 목록에 없는 입력값을 그대로 쓰는 "직접 입력" 항목.
type ModelOption = { model: string; isCustom: boolean };

const optionLabel = (option: ModelOption) => option.model;
const isSameModel = (a: ModelOption, b: ModelOption) => a.model === b.model;

// LLM 프로필의 모델 칸. 목록에서 고르는 것이 기본이고, 입력한 글자는 곧 모델값이라 목록에 없는 이름도 넣을 수 있다.
// 입력값이 목록에 없으면 "직접 입력" 항목을 보여 그 값을 그대로 쓴다는 것을 드러낸다.
export function ModelCombobox({
  id,
  value,
  onValueChange,
  options,
  isLoading,
  onReload,
  portalContainer,
  placeholder = "모델 검색·선택 또는 직접 입력",
}: {
  id: string;
  value: string;
  onValueChange: (model: string) => void;
  options: readonly string[];
  isLoading: boolean;
  onReload: () => void;
  // Sheet(Radix Dialog) 밖 <body>로 portal되면 Sheet의 포인터·스크롤 잠금에 걸리므로 Sheet 안으로 portal한다.
  portalContainer: React.RefObject<HTMLElement | null>;
  placeholder?: string;
}) {
  const typed = value.trim();
  const items = React.useMemo(() => {
    const listed: ModelOption[] = options.map((model) => ({ model, isCustom: false }));
    if (typed && !options.includes(typed)) listed.push({ model: typed, isCustom: true });
    return listed;
  }, [options, typed]);
  const selected = typed ? { model: typed, isCustom: false } : null;

  return (
    <div className="flex gap-2">
      <Combobox
        items={items}
        itemToStringLabel={optionLabel}
        isItemEqualToValue={isSameModel}
        value={selected}
        onValueChange={(option) => {
          if (option) onValueChange(option.model);
        }}
        inputValue={value}
        onInputValueChange={(next) => onValueChange(next)}
      >
        <ComboboxInput id={id} className="w-full font-mono" placeholder={placeholder} />
        <ComboboxContent portalContainer={portalContainer}>
          <ComboboxEmpty>모델 이름을 입력하세요</ComboboxEmpty>
          <ComboboxList>
            {(option: ModelOption) => (
              <ComboboxItem key={`${option.isCustom ? "custom" : "listed"}-${option.model}`} value={option}>
                {option.isCustom ? (
                  <>
                    <PencilIcon />
                    <span className="truncate">
                      직접 입력: <span className="font-mono">{option.model}</span>
                    </span>
                  </>
                ) : (
                  <span className="truncate font-mono text-xs">{option.model}</span>
                )}
              </ComboboxItem>
            )}
          </ComboboxList>
        </ComboboxContent>
      </Combobox>
      <Button
        type="button"
        variant="outline"
        size="icon"
        className="shrink-0"
        disabled={isLoading}
        onClick={onReload}
        title="API에서 쓸 수 있는 모델 다시 불러오기"
      >
        {isLoading ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
      </Button>
    </div>
  );
}
