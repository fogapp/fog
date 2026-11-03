// useHotkeys binds a page's single-key shortcuts. Every page that had them
// repeated the same guards: nothing fires with Cmd, Ctrl or Alt held, inside
// a text field, or while a dialog is open — a key typed into a filter box
// must never restart an app. Letters match either case; "Escape" fires only
// when nothing (an open dialog, say) has already handled the keystroke.

import { useEffect, useRef } from "react";

type Handlers = Record<string, (e: KeyboardEvent) => void>;

function inTextField(t: EventTarget | null) {
  const el = t as HTMLElement | null;
  return !!el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT" || el.isContentEditable);
}

export function useHotkeys(handlers: Handlers, { disabled = false }: { disabled?: boolean } = {}) {
  // The latest handlers, so callers can pass closures over fresh state
  // without the listener being re-bound each render.
  const latest = useRef(handlers);
  useEffect(() => {
    latest.current = handlers;
  });

  useEffect(() => {
    if (disabled) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || inTextField(e.target)) return;
      if (e.key === "Escape") {
        if (!e.defaultPrevented) latest.current.Escape?.(e);
        return;
      }
      latest.current[e.key.toLowerCase()]?.(e);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [disabled]);
}
