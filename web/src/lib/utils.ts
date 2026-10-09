import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// 드로어/대화 상자(Sheet/Dialog)의 onInteractOutside 닫기 판정을 돕는다.
//
// 배경: 드로어 안의 Radix 팝업 레이어(Select 드롭다운, DropdownMenu, Popover 등)는 드로어 바깥으로
// portal된다. 팝업이 열린 채로 오버레이/드로어 밖을 눌러 팝업을 접으려 하면, 이 한 번의 pointerdown을
// Select와 Sheet 두 DismissableLayer가 함께 처리한다. Select가 먼저 닫히고 discrete 이벤트라 React가 동기로 flush하므로,
// Sheet의 처리기 차례가 되면 팝업의 data-state는 이미 closed로 바뀌어 있다 — '그 순간'에 팝업이 열렸는지 검사하는 것은
// 본래 신뢰할 수 없다(실제로 확인함).
//
// 올바른 방법: Radix의 pointerdown 리스너는 버블 단계에서 돈다. 우리는 capture 단계(그보다 먼저)에서
// '지금 팝업이 열려 있는지'를 먼저 기록해 두고, onInteractOutside가 그 기록 값을 읽어 닫기를 허용할지 정한다.
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture: Radix의 버블 단계 pointerdown 처리기보다 먼저 기록한다
  );
}

// radixOverlayWasOpenAtPointerDown은 '가장 최근 pointerdown이 발생했을 때 Radix 팝업 레이어가
// 열려 있었는지'를 돌려준다. 드로어/대화 상자는 이에 따라 팝업이 열린 채 오버레이를 누르면 → 팝업만 접고 자신은 닫지 않는다.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText는 텍스트를 클립보드에 쓰고, 성공했는지 돌려준다.
// 배경: navigator.clipboard는 보안 컨텍스트(HTTPS / localhost)에서만 쓸 수 있다. IP + HTTP로
// 접근하면 undefined이므로, 이때는 execCommand("copy")로 내려간다.
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 대체 방안으로 계속 진행한다
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
