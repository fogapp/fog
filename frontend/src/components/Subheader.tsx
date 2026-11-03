// The subheader: how a page with peer views opens. The window header has
// already named the page, so the first thing under it is the tab strip on
// one hairline, with whatever belongs to the whole page — a state pill, an
// address, the page's actions — at the right end of the same line, and any
// standing remark (a stale note, a warning) as a hint line beneath.
//
// Inbox, Drive, Connect and the app page all open this way; each used
// to draw its own strip with its own inset and padding. One component, one
// gutter, one row height.

import type { ReactNode } from "react";
import Tabs, { type TabItem } from "./Tabs";
import { GUTTER } from "./Layout";

export default function Subheader<T extends string>({
  label,
  items,
  active,
  onChange,
  right,
  notes,
  gutter = GUTTER,
}: {
  // The strip's accessible name: "Inbox view", "App sections".
  label: string;
  items: readonly TabItem<T>[];
  // Query-routed strips (?tab=) pass active + onChange; segment-routed
  // ones (/apps/x/logs) pass `to` on each item and let the URL match, or
  // pass active alone when "you are here" is wider than the link.
  active?: T;
  onChange?: (id: T) => void;
  // What sits at the right end of the strip's line.
  right?: ReactNode;
  // Hint lines under the strip — see Note.
  notes?: ReactNode;
  // The strip's side inset; the page gutter unless the page keeps its own
  // (Drive runs at half of it).
  gutter?: string;
}) {
  return (
    <div className={`shrink-0 border-b border-hair ${gutter}`}>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 pt-2">
        <div className="-mb-px">
          <Tabs variant="underline" inline label={label} items={items} active={active} onChange={onChange} />
        </div>
        {right && <div className="ml-auto flex flex-wrap items-center gap-3 pb-2">{right}</div>}
      </div>
      {notes}
    </div>
  );
}

// One hint line under the strip. Quiet by default; amber when it is a
// warning the page wants read.
export function Note({ tone = "quiet", children }: { tone?: "quiet" | "warn"; children: ReactNode }) {
  return <p className={`pb-2 text-hint ${tone === "warn" ? "text-warn" : "text-ink-muted"}`}>{children}</p>;
}
