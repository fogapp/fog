// The Inbox reads like mail: what arrived down the left, the one you picked
// open beside it. Every tab is the same shape, so moving between errors,
// issues and notices never rearranges the furniture — only the rows change.
//
// The two columns scroll independently and the pane is where every fact
// lives; a row is a summary and nothing more. Under lg there is no room for
// both, so the list and the pane take turns: picking something replaces the
// list, and the pane's own back link brings it back.

import type { ReactNode } from "react";

export function InboxSplit({
  selected,
  list,
  detail,
}: {
  // Drives the small-screen swap: with nothing picked the list owns the
  // width, with something picked the pane does.
  selected: boolean;
  list: ReactNode;
  detail: ReactNode;
}) {
  return (
    <div className="flex min-h-0 flex-1">
      <div
        className={`${selected ? "hidden lg:flex" : "flex"} w-full min-w-0 shrink-0 flex-col overflow-y-auto border-hair lg:w-96 lg:border-r xl:w-[26rem]`}
      >
        {list}
      </div>
      <div className={`${selected ? "flex" : "hidden lg:flex"} min-w-0 flex-1 flex-col overflow-y-auto`}>
        {detail}
      </div>
    </div>
  );
}

// One row. The badge is whatever the kind leads with — a severity, a state —
// and `when` is the timestamp every kind has. `check` rides outside the
// button rather than inside it: a checkbox nested in a button is neither
// valid nor clickable on its own.
export function InboxRow({
  selected,
  onClick,
  badge,
  title,
  subtitle,
  when,
  tag,
  check,
}: {
  selected: boolean;
  onClick: () => void;
  badge?: ReactNode;
  title: ReactNode;
  subtitle?: ReactNode;
  when?: string;
  // A pill on the second line: the app a thing belongs to, where it came from.
  tag?: ReactNode;
  // The multi-select box, for lists that can act on several rows at once.
  check?: ReactNode;
}) {
  return (
    <div
      className={`flex items-start border-b border-divider transition-colors ${
        selected ? "bg-fog-800/70" : "hover:bg-hover"
      }`}
    >
      {check && <div className="shrink-0 py-3 pl-4">{check}</div>}
      <button
        onClick={onClick}
        aria-current={selected ? "true" : undefined}
        className={`min-w-0 flex-1 py-3 pr-4 text-left ${check ? "pl-2" : "pl-4"}`}
      >
        <div className="flex items-start gap-2">
          {badge && <span className="mt-0.5 shrink-0">{badge}</span>}
          <span className="min-w-0 flex-1 truncate text-sm font-medium text-ink">{title}</span>
          {when && <span className="shrink-0 text-[11px] text-ink-muted">{when}</span>}
        </div>
        {(subtitle || tag) && (
          <div className="mt-1 flex items-center gap-2">
            {tag}
            {subtitle && <span className="min-w-0 flex-1 truncate text-xs text-ink-muted">{subtitle}</span>}
          </div>
        )}
      </button>
    </div>
  );
}

// The pane with nothing picked. Not an error and not empty state for the
// tab — the list beside it may be full — so it says only what to do.
export function NoSelection({ label }: { label: string }) {
  return (
    <div className="flex flex-1 items-center justify-center p-8">
      <p className="text-sm text-ink-muted">{label}</p>
    </div>
  );
}

// The pane's own frame: a back link that only exists where the list is
// hidden, then the content, padded away from the divider.
export function DetailPane({ onBack, children }: { onBack: () => void; children: ReactNode }) {
  return (
    <div className="min-h-0 flex-1 px-4 py-4 sm:px-6">
      <button onClick={onBack} className="mb-3 text-sm text-ink-muted hover:text-ink lg:hidden">
        ← Back
      </button>
      {children}
    </div>
  );
}

// What a tab says when its own list is empty. Sits in the list column, so
// it has to read at that width.
export function EmptyList({ title, hint }: { title: string; hint: string }) {
  return (
    <div className="p-6 text-center">
      <p className="text-sm text-ink-muted">{title}</p>
      <p className="mt-1 text-xs text-ink-muted">{hint}</p>
    </div>
  );
}

// A pill for the app (or peer) a row belongs to.
export function Tag({ children, tone = "fog" }: { children: ReactNode; tone?: "fog" | "violet" }) {
  return (
    <span
      className={`shrink-0 rounded-full px-1.5 py-0.5 text-[10px] font-medium ${
        tone === "violet" ? "bg-accent/10 text-accent" : "bg-fog-800 text-ink-muted"
      }`}
    >
      {children}
    </span>
  );
}
