import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// 서랍/다이얼로그(Sheet/Dialog)의 onInteractOutside 닫기 판정 보조 함수.
//
// 배경: 서랍 안의 Radix 오버레이(Select 드롭다운, DropdownMenu, Popover 등)는 서랍
// 바깥으로 portal된다. 오버레이가 열린 상태에서 배경/서랍 바깥을 눌러 오버레이만 닫으려 할 때,
// 이 한 번의 pointerdown을 Select와 Sheet 두 DismissableLayer가 동시에 처리한다. Select가
// 먼저 닫히고 discrete 이벤트라 React가 동기로 flush하므로, Sheet 핸들러 차례가 되면 오버레이의
// data-state는 이미 closed로 바뀌어 있다 —— "지금 이 순간" 오버레이가 열려 있는지 검사하는 것은
// 본질적으로 신뢰할 수 없다(실측으로 확인됨).
//
// 올바른 방법: Radix의 pointerdown 리스너는 버블 단계에 있다. 우리는 capture 단계(그보다 먼저)에서
// "이 순간 오버레이가 열려 있는지"를 먼저 기록해 두고, onInteractOutside가 그 기록값을 읽어 닫기를 허용할지 판단한다.
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
    true, // capture: Radix 버블 단계의 pointerdown 핸들러보다 먼저 기록한다
  );
}

// radixOverlayWasOpenAtPointerDown 은 "가장 최근 pointerdown이 발생했을 때 Radix 오버레이가
// 열려 있었는지"를 반환한다. 서랍/다이얼로그는 이를 근거로: 오버레이가 열린 상태에서 배경을 누르면 → 오버레이만 닫고 자신은 닫지 않는다.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText 는 텍스트를 클립보드에 쓰고 성공 여부를 반환한다.
// 배경: navigator.clipboard 는 보안 컨텍스트(HTTPS / localhost)에서만 사용할 수 있다. IP + HTTP로
// 접근할 때는 undefined이므로, 이 경우 execCommand("copy")로 폴백한다.
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 폴백 방식으로 계속 진행
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
