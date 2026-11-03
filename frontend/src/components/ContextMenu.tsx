// A right-click menu: the verbs that apply to what was clicked, at the
// pointer, closing on Escape, click-away, or a choice. Positioned so it
// never runs off the viewport. The first in the codebase, so it is
// generic — items in, one callback out.

import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { useDismissable } from "../lib/useDismissable";

export interface MenuItem {
  label?: ReactNode;
  onSelect?: () => void;
  disabled?: boolean;
  danger?: boolean;
  shortcut?: string;
  // A separator: renders a rule, nothing else.
  separator?: boolean;
}

export default function ContextMenu({ x, y, items, onClose }: { x: number; y: number; items: MenuItem[]; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ left: x, top: y });
  useDismissable(ref, true, onClose);

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    setPos({
      left: Math.max(4, Math.min(x, window.innerWidth - r.width - 4)),
      top: Math.max(4, Math.min(y, window.innerHeight - r.height - 4)),
    });
  }, [x, y]);

  // Arrow keys walk the enabled items; Enter chooses.
  const [hi, setHi] = useState(-1);
  const enabled = items.map((it, i) => (!it.separator && !it.disabled ? i : -1)).filter((i) => i >= 0);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        e.stopPropagation();
        const cur = enabled.indexOf(hi);
        const next = e.key === "ArrowDown" ? enabled[(cur + 1) % enabled.length] : enabled[(cur - 1 + enabled.length) % enabled.length];
        setHi(next ?? -1);
      } else if (e.key === "Enter" && hi >= 0) {
        e.preventDefault();
        e.stopPropagation();
        items[hi].onSelect?.();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  }, [enabled, hi, items, onClose]);

  return (
    <div
      ref={ref}
      role="menu"
      style={{ left: pos.left, top: pos.top }}
      className="fixed z-50 min-w-48 rounded-lg border border-veil-edge bg-fog-900 py-1 text-sm shadow-2xl shadow-black/40"
      onContextMenu={(e) => e.preventDefault()}
    >
      {items.map((it, i) =>
        it.separator ? (
          <div key={i} className="my-1 border-t border-hair" />
        ) : (
          <button
            key={i}
            role="menuitem"
            type="button"
            disabled={it.disabled}
            onPointerMove={() => !it.disabled && setHi(i)}
            onClick={() => {
              if (it.disabled) return;
              it.onSelect?.();
              onClose();
            }}
            className={`flex w-full items-center gap-4 px-3 py-1.5 text-left disabled:opacity-40 ${
              hi === i && !it.disabled ? "bg-fog-800" : ""
            } ${it.danger ? "text-warn" : "text-ink"}`}
          >
            <span className="flex-1">{it.label}</span>
            {it.shortcut && <span className="text-xs text-ink-muted">{it.shortcut}</span>}
          </button>
        ),
      )}
    </div>
  );
}
