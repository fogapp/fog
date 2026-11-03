// Modal: the page dims and one panel takes over. It closes the three ways an
// operator will try — the ✕, the backdrop, Escape — because a dialog that
// only closes one way reads as a dialog that's stuck.
//
// A dialog is one of the few solid surfaces (fog-900): a veil over a dimmed
// page would let the page bleed through.

import { useEffect, useRef, type ReactNode } from "react";

// Open dialogs, innermost last. A folder picker opened from inside a dialog
// is a dialog on a dialog, and Escape belongs to the top one only.
const open: number[] = [];
let nextId = 0;

export default function Modal({
  title,
  onClose,
  width = "max-w-lg",
  flush = false,
  children,
}: {
  title: string;
  onClose: () => void;
  width?: string;
  // The body lays out its own edge-to-edge sections (a picker's breadcrumb,
  // list and footer) instead of sitting in the padded scroll area.
  flush?: boolean;
  children: ReactNode;
}) {
  // The latest onClose, so a caller's inline closure doesn't re-subscribe
  // the listener — and re-stack this dialog — on every render.
  const close = useRef(onClose);
  useEffect(() => {
    close.current = onClose;
  });

  useEffect(() => {
    const id = ++nextId;
    open.push(id);
    // Escape is the dialog's: it closes, and marks the event handled so a
    // page-level Escape (clear a selection, say) does not fire on the same
    // keystroke the moment the dialog is gone.
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !e.defaultPrevented && open[open.length - 1] === id) {
        e.preventDefault();
        close.current();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => {
      open.splice(open.indexOf(id), 1);
      document.removeEventListener("keydown", onKey);
    };
  }, []);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4" onClick={onClose}>
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
        className={`flex max-h-[80vh] w-full ${width} flex-col rounded-xl border border-veil-edge bg-fog-900 shadow-2xl shadow-black/50`}
      >
        <div className="flex items-center justify-between border-b border-hair px-4 py-3">
          <h2 className="text-sm font-medium text-ink">{title}</h2>
          <button onClick={onClose} className="rounded px-1 text-ink-muted hover:text-ink" title="Close">
            ✕
          </button>
        </div>
        {flush ? children : <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">{children}</div>}
      </div>
    </div>
  );
}
