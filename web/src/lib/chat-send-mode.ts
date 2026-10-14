"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// Send / newline keybinding for the chat composer. A frontend-only preference: it
// lives in localStorage, is not stored in the database, and does not sync with the
// account, so a different browser needs it set again. See issue #39 — 0.3.2 changed
// Ctrl+Enter send to Enter send, and this brings the old binding back as an option.
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter to send, Shift+Enter for a new line" },
  { value: "ctrl-enter", label: "Ctrl+Enter to send, Enter for a new line" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// Subscribers in the same tab. The localStorage storage event only fires in other
// tabs, so a change made in Settings has to notify this tab's composer via emit,
// or the composer would not update until a refresh.
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// Returns a string literal. Object.is compares by value, so useSyncExternalStore does not loop.
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// The server has no localStorage. Render the default first; getSnapshot corrects it after hydrate.
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey decides whether this keypress should send.
// isComposing / keyCode 229 means an IME (for example a Chinese input method) is
// still composing, so the key must pass through — otherwise Enter would send while
// the user is picking a candidate.
// enter mode only excludes Shift, matching 0.3.2 exactly so users who never change
// the setting keep the same feel.
// ctrl-enter mode accepts both Ctrl and Cmd (macOS).
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
