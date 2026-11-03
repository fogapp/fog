// The one line that replaces a confirm dialog: the thing already
// happened, here is how to undo it, and it goes away by itself. Delete
// without asking is only honest when undo is this close.

import { useEffect } from "react";

export interface ToastState {
  message: string;
  undo?: () => void;
  tone?: "ok" | "error";
}

export default function Toast({ toast, onDone, ms = 8000 }: { toast: ToastState | null; onDone: () => void; ms?: number }) {
  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(onDone, toast.tone === "error" ? Math.max(ms, 10000) : ms);
    return () => clearTimeout(t);
  }, [toast, onDone, ms]);
  if (!toast) return null;
  return (
    <div
      role="status"
      className={`fixed bottom-4 left-1/2 z-40 flex -translate-x-1/2 items-center gap-3 rounded-lg border px-4 py-2.5 text-sm shadow-2xl shadow-black/40 ${
        toast.tone === "error" ? "border-warn/30 bg-warn/10 text-warn" : "border-edge bg-veil text-ink"
      }`}
    >
      <span>{toast.message}</span>
      {toast.undo && (
        <button
          type="button"
          onClick={() => {
            toast.undo?.();
            onDone();
          }}
          className="rounded-md border border-fog-600 px-2 py-0.5 text-xs font-semibold text-ink hover:bg-fog-800"
        >
          Undo
        </button>
      )}
      <button type="button" onClick={onDone} className="text-ink-muted hover:text-ink" title="Dismiss">
        ✕
      </button>
    </div>
  );
}
