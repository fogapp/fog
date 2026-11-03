// The top of a page: what it is, one line on why, the actions that belong to
// the whole page, and — when the page has peer views — the tab strip under
// them. Pages that used to open with a bare <h1> and a paragraph use this so
// the title, its description, and its actions sit in the same place on every
// page, instead of each one deciding where.
//
// The title carries the page by weight, not size: 22px at 700, tight. A
// page that sits inside a frame of its own (a Settings page, beside its
// submenu) asks for the smaller size, so the frame stays the larger thing.
// Where you are in fog is the shell header's job.

import type { ReactNode } from "react";

export default function PageHeader({
  title,
  description,
  badge,
  actions,
  size = "lg",
  children,
}: {
  title: ReactNode;
  description?: ReactNode;
  // Rides beside the title: a state pill, a saved tick.
  badge?: ReactNode;
  actions?: ReactNode;
  size?: "lg" | "md";
  // The tab strip (or anything else) that belongs under the header.
  children?: ReactNode;
}) {
  return (
    <div className="space-y-5">
      <div className={`flex flex-wrap justify-between gap-x-6 gap-y-3 ${size === "lg" ? "items-end" : "items-start sm:flex-nowrap"}`}>
        <div className="min-w-0">
          <h1 className={`flex flex-wrap items-center gap-3 font-bold text-ink ${size === "lg" ? "text-title" : "text-xl leading-tight tracking-[-.01em]"}`}>
            <span className="min-w-0 break-words">{title}</span>
            {badge}
          </h1>
          {description && <p className="mt-1 text-sm text-ink-muted">{description}</p>}
        </div>
        {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {children}
    </div>
  );
}
