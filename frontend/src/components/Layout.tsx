import { useEffect, useState, type ReactNode } from "react";
import { Link, Outlet, useLocation } from "react-router-dom";
import InboxBell from "./InboxBell";
import FeedbackButton from "./FeedbackButton";
import AvatarMenu from "./AvatarMenu";
import Sidebar from "./Sidebar";
import { Menu, Search } from "lucide-react";
import CommandPalette, { openCommandPalette } from "./CommandPalette";
import DeployTray from "./DeployTray";
import { crumbsFor } from "../lib/breadcrumb";

// The shell: a fixed full-height sidebar (the map of fog) beside a
// full-width main column with a slim header. The sidebar says WHERE in fog;
// the header says where you are and who you are; the page says everything
// about the thing itself — including how much room it wants around it.
//
// Both columns sit directly on the flat ground, split by a hairline:
// the shell paints no surface of its own.

// The page gutter, for routes that want the ordinary inset. Full-bleed
// pages skip it; anything drawing its own edge-to-edge band inside a
// padded page matches these values by hand.
const PAGE_PAD = "px-4 py-6 sm:px-6 lg:px-8";
// The same inset without the vertical part: what a full-bleed page's own
// bands (a subheader, a toolbar) carry so they line up with the crumb.
export const GUTTER = "px-4 sm:px-6 lg:px-8";

// The sidebar's width, shared with the main column's inset.
const SIDEBAR_W = "w-[218px]";
const SIDEBAR_PL = "lg:pl-[218px]";

// Wraps a route that wants the ordinary inset. Applied in App's route
// table rather than inside each page, so which pages bleed and which don't
// can be read off one list.
export function Page({ children }: { children: ReactNode }) {
  return <div className={PAGE_PAD}>{children}</div>;
}

export default function Layout({ handle, basic }: { handle: string; basic?: boolean }) {
  const location = useLocation();
  const [drawer, setDrawer] = useState(false);

  useEffect(() => setDrawer(false), [location.pathname]);

  return (
    <div className="min-h-screen">
      <CommandPalette basic={basic} />
      <aside className={`fixed inset-y-0 left-0 z-40 hidden ${SIDEBAR_W} border-r border-hair lg:block`}>
        <Sidebar basic={basic} />
      </aside>

      {drawer && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div className="absolute inset-0 bg-black/60" onClick={() => setDrawer(false)} />
          <aside className={`absolute inset-y-0 left-0 ${SIDEBAR_W} border-r border-hair bg-fog-950`}>
            <Sidebar basic={basic} />
          </aside>
        </div>
      )}

      <div className={`flex min-h-screen flex-col ${SIDEBAR_PL}`}>
        <header className={`sticky top-0 z-30 flex h-14 shrink-0 items-center gap-3 border-b border-hair bg-fog-950/70 backdrop-blur ${GUTTER}`}>
          <button
            onClick={() => setDrawer(true)}
            aria-label="Open navigation"
            className="flex size-8 items-center justify-center rounded-md text-ink-muted hover:bg-hover hover:text-ink lg:hidden"
          >
            <Menu className="size-4" aria-hidden />
          </button>
          <Breadcrumb parts={crumbsFor(location.pathname, location.search, { basic: !!basic })} />
          <div className="ml-auto flex items-center gap-3">
            <SearchBox />
            {!basic && (
              <div className="flex items-center gap-1">
                <FeedbackButton />
                <InboxBell />
              </div>
            )}
            <AvatarMenu handle={handle} basic={basic} />
          </div>
        </header>
        {/* A bare frame. Padding belongs to the page, not the shell: the
            ones that want to run to the edges — Home's table, the Inbox's
            two columns, Files — used to cancel this with negative margins,
            and the rest ask for it by name in App's route table. */}
        <main className="min-w-0 flex-1">
          <Outlet />
        </main>
      </div>
      {!basic && <DeployTray />}
    </div>
  );
}

// The way into the command palette, for anyone who doesn't know ⌘K: a quiet
// box that says what it does and which keys do it faster.
function SearchBox() {
  const mac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);
  return (
    <button
      type="button"
      onClick={openCommandPalette}
      className="hidden h-8 w-56 items-center gap-2 rounded-md border border-edge bg-veil px-2.5 text-sm text-ink-faint transition-colors hover:border-edge-hover hover:text-ink-muted md:flex"
    >
      <Search className="size-4 shrink-0" aria-hidden />
      <span className="flex-1 text-left">Search</span>
      <kbd className="rounded border border-hair bg-hover px-1.5 font-sans text-[11px] font-medium text-ink-muted">
        {mac ? "⌘K" : "Ctrl K"}
      </kbd>
    </button>
  );
}

// Where you are: "jellyfin / Logs", the page itself in ink and the levels
// above it as quiet links.
function Breadcrumb({ parts }: { parts: { label: string; to?: string }[] }) {
  return (
    <nav aria-label="Breadcrumb" className="flex min-w-0 items-center gap-2 text-sm">
      {parts.map((p, i) => (
        <span key={i} className="flex min-w-0 items-center gap-2">
          {i > 0 && (
            <span aria-hidden className="text-ink-faint">
              /
            </span>
          )}
          {p.to ? (
            <Link to={p.to} className="truncate text-ink-muted hover:text-ink">
              {p.label}
            </Link>
          ) : (
            <span className="truncate font-semibold text-ink">{p.label}</span>
          )}
        </span>
      ))}
    </nav>
  );
}
