// The sidebar is the map of fog, and it is the same map on every page:
// Home, Inbox, Chat, and Drive, every app, every peer under the Connect
// heading, and the Settings areas. Where you are inside an app is the app page's business — its tabs —
// so entering an app never redraws this column into something else.
//
// Basic mode keeps the one thing a household navigates by: the app list.

import { useEffect, useMemo, useRef, useState } from "react";
import { FolderClosed, House, Inbox, MessageSquare, Search, Settings, type LucideIcon } from "lucide-react";
import { Link, NavLink, useLocation, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { useApps } from "../lib/useApps";
import { useInboxCounts } from "../lib/useInbox";
import { filesRelFromPath, paths } from "../lib/paths";
import { SETTINGS_AREAS } from "../lib/settings";
import { useFilePins, usePinActions } from "../lib/useFilePins";
import { peerLabel, versionLabel } from "../lib/format";
import { Dot } from "../pages/app/cockpit";
import { peerTone, stateWord } from "../pages/federation/data";
import Badge from "./Badge";
import Logo from "./Logo";
import { AppUpdatesBadge, StaleBackupsBadge } from "./SettingsBadges";
import { StateDot } from "./StateBadge";
import { federationQuery, systemQuery } from "../queries";

// A nav row: 14px at the control height, an icon (or an app's state dot)
// before the name. The active row is the beacon: its tint behind, its
// colour on the words.
const navClass = ({ isActive }: { isActive: boolean }) =>
  `flex h-8 items-center gap-2.5 rounded-md px-2.5 text-sm transition-colors ${
    isActive ? "bg-accent/10 font-medium text-accent" : "text-ink-muted hover:bg-hover hover:text-ink"
  }`;

// A row under a row — Drive's pins, the Settings areas — lines its words up
// with the parent's, past where the parent's icon sits.
const subClass = ({ isActive }: { isActive: boolean }) =>
  `block rounded-md py-1.5 pl-[34px] pr-2.5 text-cell transition-colors ${
    isActive ? "bg-accent/10 font-medium text-accent" : "text-ink-muted hover:bg-hover hover:text-ink"
  }`;

function NavIcon({ icon: Icon }: { icon: LucideIcon }) {
  return <Icon className="size-4 shrink-0 opacity-80" aria-hidden />;
}

// A group's heading is a link like any other row — the same rounded hover,
// the same tint when it is where you are — set small and quiet so it reads
// as a heading over the rows. Quiet words alone read as a label, not a door,
// so every heading carries a glyph that says what a click does: › opens the
// page it names, + adds one. The glyph brightens with the row on hover. Its tools
// (a filter) sit beside it as their own square buttons, each lit on its own
// hover, so what a click will do is never ambiguous.
function GroupHeading({ to, label, title, active, glyph = "›", children }: {
  to: string;
  label: string;
  title?: string;
  active: boolean;
  glyph?: "›" | "+";
  children?: React.ReactNode;
}) {
  return (
    <div className="mb-0.5 flex items-center gap-0.5">
      <Link
        to={to}
        title={title}
        aria-current={active ? "page" : undefined}
        className={`group flex h-7 flex-1 items-center rounded-md px-2.5 text-xs font-medium transition-colors ${
          active ? "bg-accent/10 text-accent" : "text-ink-faint hover:bg-hover hover:text-ink"
        }`}
      >
        <span className="flex-1">{label}</span>
        <span
          aria-hidden
          className={`text-[13px] leading-none transition-colors ${
            active ? "text-accent" : "text-ink-faint group-hover:text-ink"
          }`}
        >
          {glyph}
        </span>
      </Link>
      {children}
    </div>
  );
}

const headingTool = (on: boolean) =>
  `flex h-7 w-7 shrink-0 items-center justify-center rounded-md leading-none transition-colors hover:bg-hover hover:text-ink ${
    on ? "text-ink" : "text-ink-muted"
  }`;

export default function Sidebar({ basic }: { basic?: boolean }) {
  return (
    <div className="flex h-full flex-col">
      <Link
        to={paths.home}
        className="flex h-14 shrink-0 items-center gap-2.5 px-5 text-[17px] font-semibold text-ink"
      >
        <Logo className="h-5" />
        fog
      </Link>

      <nav className="flex-1 space-y-6 overflow-y-auto px-2 pb-4 pt-1">
        {!basic && (
          <div className="space-y-0.5">
            <NavLink to={paths.home} end className={navClass}>
              <NavIcon icon={House} />
              Home
            </NavLink>
            <InboxLink />
            <NavLink to={paths.chat()} className={navClass}>
              <NavIcon icon={MessageSquare} />
              Chat
            </NavLink>
            <NavLink to={paths.files()} className={navClass}>
              <NavIcon icon={FolderClosed} />
              Drive
            </NavLink>
            <DrivePins />
          </div>
        )}

        <AppList />

        {!basic && <PeerList />}

        {!basic && <SettingsGroup />}
      </nav>

      {!basic && <Footer />}
    </div>
  );
}

// What you pinned in Drive, under Drive. A folder opens; a file opens its
// folder with the file selected. A pin whose entry is gone — trashed, or
// moved outside fog — stays, struck through, until it is restored or
// unpinned: silently dropping it would lose a restored file's pin.
function DrivePins() {
  const pins = useFilePins();
  const { unpin } = usePinActions();
  const location = useLocation();
  const here = filesRelFromPath(location.pathname);
  const list = pins.data ?? [];
  if (list.length === 0) return null;
  return (
    <div className="space-y-0.5">
      {list.map((p) => {
        const slash = p.path.lastIndexOf("/");
        const parent = slash < 0 ? "" : p.path.slice(0, slash);
        const to = p.kind === "dir" ? paths.files(p.path) : `${paths.files(parent)}?select=${encodeURIComponent(p.name)}`;
        const active = p.kind === "dir" && here === p.path;
        return (
          <div key={p.path} className="group relative">
            {p.missing ? (
              <span
                className="block truncate rounded-md py-1.5 pl-[34px] pr-7 text-cell text-ink-faint line-through"
                title={`${p.path} — no longer there: in the Trash, or moved outside Drive. Restore it, or unpin it.`}
              >
                {p.name}
              </span>
            ) : (
              <Link to={to} title={p.path} aria-current={active ? "page" : undefined} className={`${subClass({ isActive: active })} truncate pr-7`}>
                {p.name}
              </Link>
            )}
            <button
              type="button"
              onClick={() => unpin.mutate(p.path)}
              aria-label={`Unpin ${p.name}`}
              title="Unpin"
              className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded px-1 leading-none text-ink-muted opacity-0 transition-opacity hover:bg-hover hover:text-ink focus:opacity-100 group-hover:opacity-100"
            >
              ×
            </button>
          </div>
        );
      })}
    </div>
  );
}

function InboxLink() {
  const counts = useInboxCounts();
  return (
    <NavLink to={paths.inbox()} className={navClass}>
      <NavIcon icon={Inbox} />
      <span className="flex-1">Inbox</span>
      <Badge count={counts.total} hot={counts.hot} label="waiting in the inbox" />
    </NavLink>
  );
}

// Every app, straight in the sidebar, with the two facts worth a glyph: an
// update waiting and open errors. The heading opens Add app. Past a
// handful of apps a magnifier appears beside it — typing is faster than scrolling, but a box
// standing open all day is one more thing in a column that is meant to be
// read at a glance, so the box arrives only when the glass is clicked.
const FILTER_FROM = 8;

function AppList() {
  const apps = useApps({ refetchInterval: 15000 });
  const [q, setQ] = useState("");
  const [filtering, setFiltering] = useState(false);
  const box = useRef<HTMLInputElement>(null);
  const location = useLocation();
  // A filter is for finding the next app; it should not follow you into it.
  useEffect(() => {
    setQ("");
    setFiltering(false);
  }, [location.pathname]);
  // The glass opens the box and puts the cursor in it: one click, then type.
  useEffect(() => {
    if (filtering) box.current?.focus();
  }, [filtering]);

  const close = () => {
    setQ("");
    setFiltering(false);
  };

  const list = useMemo(() => {
    const all = apps.data ?? [];
    const needle = q.trim().toLowerCase();
    return needle ? all.filter((a) => a.name.toLowerCase().includes(needle)) : all;
  }, [apps.data, q]);

  return (
    <div>
      {/* The heading is the way in to Add app — no separate + beside it. */}
      <GroupHeading to={paths.newApp} label="Apps" title="Add app" glyph="+" active={location.pathname === paths.newApp}>
        {(apps.data?.length ?? 0) >= FILTER_FROM && (
          <button
            type="button"
            onClick={() => (filtering ? close() : setFiltering(true))}
            aria-label="Filter apps"
            aria-expanded={filtering}
            aria-controls="app-filter"
            title="Filter apps"
            className={headingTool(filtering)}
          >
            <Search className="size-3.5" aria-hidden />
          </button>
        )}
      </GroupHeading>
      {filtering && (
        <input
          id="app-filter"
          ref={box}
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => e.key === "Escape" && close()}
          onBlur={() => q.trim() === "" && setFiltering(false)}
          placeholder="Filter apps…"
          aria-label="Filter apps"
          className="mb-1.5 h-7 w-full rounded-md border border-edge bg-veil px-2.5 text-xs text-ink placeholder-ink-faint outline-none focus:border-accent"
        />
      )}
      {apps.isLoading ? (
        <p className="px-2.5 py-1.5 text-sm text-ink-muted">Loading apps…</p>
      ) : apps.isError ? (
        <p className="px-2.5 py-1.5 text-sm text-warn">Couldn't load apps.</p>
      ) : (apps.data ?? []).length === 0 ? (
        <p className="px-2.5 py-1.5 text-sm text-ink-muted">No apps yet.</p>
      ) : list.length === 0 ? (
        <p className="px-2.5 py-1.5 text-sm text-ink-muted">Nothing matches “{q.trim()}”.</p>
      ) : (
        <div className="space-y-0.5">
          {list.map((a) => (
            <NavLink key={a.name} to={paths.app(a.name)} className={navClass}>
              <span className="flex size-4 shrink-0 items-center justify-center">
                <StateDot app={a} />
              </span>
              <span className="min-w-0 flex-1 truncate">{a.name}</span>
              {a.updateAvailable && (
                <span aria-label="update available" title="Update available" className="text-[10px] text-accent">
                  ▲
                </span>
              )}
              <Badge count={a.openErrors ?? 0} hot label="open errors" />
            </NavLink>
          ))}
        </div>
      )}
    </div>
  );
}

// Every peer, with its state dot, under a heading that opens the
// Connect page. A row selects that peer on the page's Peers tab — the
// same selection the map and the table share — so a peer in trouble is one
// click from its detail. With no peers yet the heading stands alone: it is
// still the way to the page where the first one is added.
function PeerList() {
  const fed = useQuery({
    ...federationQuery,
    staleTime: 30_000,
    refetchInterval: 30_000,
  });
  const location = useLocation();
  const [params] = useSearchParams();
  const peers = fed.data?.peers ?? [];
  const onPage = location.pathname === paths.federation;
  return (
    <div>
      <GroupHeading to={paths.federation} label="Connect" active={onPage && !params.get("peer")} />
      <div className="space-y-0.5">
        {peers.map((p) => {
          const on = onPage && params.get("peer") === String(p.id);
          return (
            <Link key={p.id} to={paths.federationView("peers", { peer: String(p.id) })} className={navClass({ isActive: on })} title={stateWord(p)}>
              <span className="min-w-0 flex-1 truncate">{peerLabel(p)}</span>
              <Dot tone={peerTone(p)} />
            </Link>
          );
        })}
      </div>
    </div>
  );
}

// Settings, as a row like Home or Drive — it is a page, the overview of
// everything the server runs on — with its five areas under it the way
// Drive's pins sit under Drive. Five rows need no fold, so there is none:
// what is in Settings is always one glance away.
function SettingsGroup() {
  return (
    <div className="space-y-0.5">
      <NavLink to={paths.settings()} end className={navClass}>
        <NavIcon icon={Settings} />
        Settings
      </NavLink>
      {SETTINGS_AREAS.map((a) => (
        <NavLink key={a.id} to={paths.settings(a.id)} className={(s) => `${subClass(s)} flex items-center gap-2`}>
          <span className="flex-1">{a.label}</span>
          {a.id === "storage" && <StaleBackupsBadge />}
          {a.id === "server" && <AppUpdatesBadge />}
        </NavLink>
      ))}
    </div>
  );
}

// The host facts that used to headline the dashboard, as ambient chrome at
// the foot of the map. Rendered only once the data exists — a line that
// says "fogd …" must be quoting the server.
function Footer() {
  const system = useQuery({
    ...systemQuery,
    staleTime: 60_000,
  });
  const d = system.data;
  if (!d) return null;
  return (
    <Link
      to={paths.settings("server/about")}
      className="block border-t border-hair px-5 py-3 text-hint leading-relaxed text-ink-muted hover:text-ink-muted"
      title={`state in ${d.dataDir}`}
    >
      fogd {versionLabel(d.version)} · {d.os}/{d.arch}
      {d.docker.available && d.docker.version ? ` · Docker ${d.docker.version.Version}` : ""}
    </Link>
  );
}
