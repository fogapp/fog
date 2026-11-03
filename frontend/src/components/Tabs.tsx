// One tab strip for every "peer views of the same thing" navigation. Two
// looks — pills for a strip inside a page, an underline row for the strip
// that sits under a page header — and one behaviour: the active tab is
// whatever the URL says, so a refresh or a shared link lands on the right
// view. Filters are not tabs; they stay chips (see the error tables).

import type { ComponentType } from "react";
import { Link, NavLink, useSearchParams } from "react-router-dom";
import Badge from "./Badge";

export interface TabItem<T extends string = string> {
  id: T;
  label: string;
  // A 16px line icon before the label (lucide-react).
  icon?: ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  count?: number;
  hot?: boolean;
  // For segment-routed tabs (the app page): the link target. Query-routed
  // tabs (?tab=) leave this unset and use onChange instead.
  to?: string;
  // Segment tabs only: match the URL exactly (the index tab).
  end?: boolean;
}

export default function Tabs<T extends string>({
  items,
  active,
  onChange,
  variant = "pills",
  label,
  inline = false,
}: {
  items: readonly TabItem<T>[];
  active?: T;
  onChange?: (id: T) => void;
  variant?: "pills" | "underline";
  label: string;
  // Inline: the strip sits in a row that draws its own hairline (the app
  // page's subheader), so it brings no border of its own.
  inline?: boolean;
}) {
  // The active tab is lit the way the active sidebar row is: the beacon,
  // as a tint behind a pill or the line under an underline tab.
  const pill = (on: boolean) =>
    `flex h-7 items-center gap-1.5 rounded-md px-2.5 text-cell font-medium transition-colors ${
      on ? "bg-accent/10 text-accent" : "text-ink-muted hover:bg-hover hover:text-ink"
    }`;
  const underline = (on: boolean) =>
    `flex items-center gap-1.5 border-b-2 pb-2.5 pt-1 text-sm font-medium transition-colors ${
      on ? "border-accent text-ink" : "border-transparent text-ink-muted hover:text-ink"
    }`;
  const cls = variant === "pills" ? pill : underline;
  const wrap =
    variant === "pills"
      ? `flex flex-wrap gap-1 ${inline ? "" : "border-b border-hair pb-3"}`
      : `flex flex-wrap gap-x-5 ${inline ? "" : "border-b border-hair"}`;

  const body = (t: TabItem<T>) => (
    <>
      {t.icon && <t.icon className="size-4 shrink-0 opacity-80" aria-hidden />}
      {t.label}
      {t.count !== undefined && <Badge count={t.count} hot={t.hot} />}
    </>
  );

  return (
    <nav aria-label={label} className={wrap}>
      {items.map((t) => {
        if (!t.to) {
          return (
            <button
              key={t.id}
              type="button"
              onClick={() => onChange?.(t.id)}
              aria-current={t.id === active ? "page" : undefined}
              className={cls(t.id === active)}
            >
              {body(t)}
            </button>
          );
        }
        // Links whose "you are here" is wider than the link itself — Drive's
        // Files tab stays lit inside every folder it points past — come with
        // `active` and settle it themselves; the rest let the URL match.
        if (active !== undefined) {
          return (
            <Link key={t.id} to={t.to} aria-current={t.id === active ? "page" : undefined} className={cls(t.id === active)}>
              {body(t)}
            </Link>
          );
        }
        return (
          <NavLink key={t.id} to={t.to} end={t.end} className={({ isActive }) => cls(isActive)}>
            {body(t)}
          </NavLink>
        );
      })}
    </nav>
  );
}

// The ?tab= half of a query-routed strip. The first id is the default and is
// written as the absence of the param, so the canonical URL for the default
// view stays clean. Switching tabs drops the other params by default — a
// selection made on one tab (?g=, ?show=) has no meaning on the next — unless
// the caller asks to keep them.
export function useTabParam<T extends string>(
  ids: readonly T[],
  opts?: { key?: string; keepParams?: boolean },
): [T, (id: T) => void] {
  const key = opts?.key ?? "tab";
  const [params, setParams] = useSearchParams();
  const raw = params.get(key);
  const active = (ids as readonly string[]).includes(raw ?? "") ? (raw as T) : ids[0];
  const set = (id: T) => {
    const next = opts?.keepParams ? new URLSearchParams(params) : new URLSearchParams();
    if (id === ids[0]) next.delete(key);
    else next.set(key, id);
    setParams(next, { replace: true });
  };
  return [active, set];
}
