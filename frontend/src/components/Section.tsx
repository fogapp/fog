// The section: how every group of controls on every screen opens. Each is
// a <section id> so a link from anywhere in fog — an issue's fix, a bell,
// a paragraph of help — can land on the exact control instead of the top
// of the page; the Settings frame scrolls a URL's #hash into view.

import type { ComponentType, ReactNode } from "react";

// How a group opens: a title in sentence case, set by weight rather than
// size, with an optional icon before it and the group's actions — or a
// right-hand fact, a count, a state — at the far end of the same line. An
// optional line of description sits under the title. No rule, no tick, no
// card: the space above the next title is what separates one group from
// another.
export function SectionHead({
  title,
  icon: Icon,
  description,
  href,
  right,
  className = "",
}: {
  title: ReactNode;
  // A 16px line icon before the title (lucide-react).
  icon?: ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  // One line on what the group is for, under the title.
  description?: ReactNode;
  // Wrap the title in a link — a Section links to its own anchor.
  href?: string;
  // What sits at the end of the line: a count, a state, the actions.
  right?: ReactNode;
  className?: string;
}) {
  return (
    <div className={className}>
      <div className="flex min-h-8 flex-wrap items-center gap-x-3 gap-y-2">
        <span className="flex min-w-0 items-center gap-2 text-heading font-semibold text-ink">
          {Icon && <Icon className="size-4 shrink-0 text-ink-muted" aria-hidden />}
          {href ? (
            <a href={href} className="min-w-0 hover:text-accent">
              {title}
            </a>
          ) : (
            <span className="min-w-0">{title}</span>
          )}
        </span>
        {right && <div className="ml-auto flex shrink-0 flex-wrap items-center gap-2">{right}</div>}
      </div>
      {description && <p className="mt-0.5 max-w-2xl text-sm text-ink-muted">{description}</p>}
    </div>
  );
}

// One block of a page. scroll-mt keeps the heading clear of the
// sticky header when a hash link lands on it.
export function Section({
  id,
  title,
  icon,
  description,
  actions,
  children,
}: {
  id: string;
  title: string;
  icon?: ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section id={id} className="scroll-mt-20">
      <h2 className="m-0">
        <SectionHead title={title} icon={icon} href={`#${id}`} right={actions} />
      </h2>
      {description && <p className="mt-0.5 max-w-2xl text-sm text-ink-muted">{description}</p>}
      <div className="mt-4 space-y-4">{children}</div>
    </section>
  );
}

// A status in one line: a dot and the answer. ok is a working setup, warn
// something half-done or erroring, idle nothing configured — which is a
// normal state here, not a fault. A Settings page wears one under its
// title, so "is this set up, and to what?" is answered before any control.
export function StatusLine({ tone = "idle", children }: { tone?: "ok" | "warn" | "idle"; children: ReactNode }) {
  const [dot, text] =
    tone === "ok"
      ? ["bg-accent", "text-accent"]
      : tone === "warn"
        ? ["bg-warn", "text-warn"]
        : ["bg-fog-600", "text-ink-muted"];
  return (
    <p className={`flex items-center gap-2 text-sm ${text}`}>
      <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${dot}`} aria-hidden />
      <span className="min-w-0">{children}</span>
    </p>
  );
}
