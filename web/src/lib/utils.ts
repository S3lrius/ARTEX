import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// Helper for the Sheet/Dialog onInteractOutside close decision.
//
// Background: Radix overlays inside a drawer (Select, DropdownMenu, Popover, and so on)
// portal outside the drawer. A pointerdown on the backdrop or outside the drawer, meant
// only to dismiss that overlay, is handled by both the Select and Sheet DismissableLayers.
// Select closes first and the event is discrete, so React flushes synchronously. By the
// time the Sheet handler runs, the overlay's data-state is already closed — checking
// "is an overlay open right now" is unreliable (confirmed in practice).
//
// Approach: Radix listens for pointerdown in the bubble phase. We record "was an overlay
// open at this moment" in the capture phase, before Radix runs, and onInteractOutside
// reads that recorded value to decide whether the close should proceed.
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
    true, // capture: record this before Radix's bubble-phase pointerdown handler
  );
}

// radixOverlayWasOpenAtPointerDown reports whether a Radix overlay was open at the
// most recent pointerdown. Drawers and dialogs use it so a backdrop click while an
// overlay is open dismisses only the overlay.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText writes text to the clipboard and reports whether it succeeded.
// navigator.clipboard is only available in a secure context (HTTPS / localhost).
// Over IP + HTTP it is undefined, so we fall back to execCommand("copy").
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Fall through to the legacy copy path.
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
