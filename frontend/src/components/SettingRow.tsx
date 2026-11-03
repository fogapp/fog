// One setting as a row: what it is and what it does on the left, the
// control on the right — a switch, a short field, a choice. Rows stack in a
// SettingRows veil, a hairline between each, so a page of preferences reads
// as one list rather than a stack of forms. On a phone the control drops
// under its words.

import type { ReactNode } from "react";

export function SettingRows({ children }: { children: ReactNode }) {
  return <div className="divide-y divide-divider rounded-lg border border-veil-edge bg-veil px-[18px]">{children}</div>;
}

export default function SettingRow({ label, hint, error, children }: {
  label: ReactNode;
  hint?: ReactNode;
  // Why the last change didn't take, under the words it is about.
  error?: string;
  children: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-3 py-4 sm:flex-row sm:items-center sm:justify-between sm:gap-6">
      <div className="min-w-0">
        <div className="text-cell text-ink">{label}</div>
        {hint && <div className="mt-[5px] text-hint text-ink-muted">{hint}</div>}
        {error && <div className="mt-[5px] text-hint text-warn">{error}</div>}
      </div>
      <div className="shrink-0">{children}</div>
    </div>
  );
}

// A list of things the page manages — keys, shares, passwords, people —
// one veil, a hairline between rows. Each row is the thing's name and its
// facts on the left and its own actions on the right.
export function RowList({ children }: { children: ReactNode }) {
  return <ul className="divide-y divide-divider/60 rounded-lg border border-veil-edge bg-veil">{children}</ul>;
}

export function RowItem({ title, meta, actions, children }: {
  title: ReactNode;
  // The facts under the name, in the hint voice.
  meta?: ReactNode;
  actions?: ReactNode;
  // Anything that belongs to the row below its line: a bar, an error.
  children?: ReactNode;
}) {
  return (
    <li className="space-y-2.5 px-4 py-3">
      <div className="flex items-center gap-3">
        <div className="min-w-0 flex-1">
          <div className="text-sm font-medium text-ink">{title}</div>
          {meta && <div className="mt-0.5 text-xs text-ink-muted">{meta}</div>}
        </div>
        {actions && <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">{actions}</div>}
      </div>
      {children}
    </li>
  );
}
