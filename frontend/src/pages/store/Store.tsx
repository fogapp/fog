// The store's front page, and the switch to one app's listing. Browsing is
// a search box, the categories as pills, a featured row of the apps with
// pictures, and every category as a section of cards — each card the
// icon, the name, one line, and what pressing it will get you: an
// install, an app that is already here, or the reason it can't run.
//
// Only enabled entries appear; which ones is decided in Settings ›
// Preferences › App store. ?app=<id> opens a listing, so a card, a companion, or a link from
// anywhere else lands on the same page.

import { useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { Capacity, CatalogEntry, CatalogResponse } from "../../api";
import AppIcon from "../../components/AppIcon";
import CapacityBar from "../../components/CapacityBar";
import { LoadError, Loading } from "../../components/Async";
import { SectionHead } from "../../components/Section";
import Tabs from "../../components/Tabs";
import { useApps } from "../../lib/useApps";
import { TONE_TEXT, approx, cardLine, useCapacity } from "../../lib/useCapacity";
import { paths } from "../../lib/paths";
import Listing from "./Listing";
import SettingsLink from "../../components/SettingsLink";
import { catalogQuery } from "../../queries";
import { control } from "../../components/Field";
import { buttonClass } from "../../components/Button";

const ALL = "all";

export default function Store() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const appId = params.get("app");
  const catalog = useQuery({
    ...catalogQuery,
  });
  const apps = useApps();
  const installed = useMemo(() => new Set((apps.data ?? []).map((a) => a.name)), [apps.data]);

  if (!catalog.data) {
    return catalog.isError
      ? <LoadError label="Couldn't load the app catalog." error={catalog.error} onRetry={() => catalog.refetch()} />
      : <Loading label="Loading the app catalog…" />;
  }

  if (appId) {
    const entry = catalog.data.entries.find((e) => e.id === appId);
    if (!entry) {
      return (
        <div className="space-y-3">
          <p className="text-sm text-ink-muted">There's no app called “{appId}” in the catalog.</p>
          <Link to={paths.newApp} className="text-sm text-accent hover:underline">← App store</Link>
        </div>
      );
    }
    return (
      <Listing
        entry={entry}
        all={catalog.data.entries}
        installed={installed}
        onBack={() => navigate(paths.newApp)}
      />
    );
  }

  return <Browse catalog={catalog.data} installed={installed} />;
}

function Browse({ catalog, installed }: { catalog: CatalogResponse; installed: Set<string> }) {
  const [search, setSearch] = useState("");
  const [category, setCategory] = useState(ALL);
  const capacity = useCapacity();
  const fits = capacity.data?.fits ?? {};

  const enabled = useMemo(() => {
    const on = new Set(catalog.enabled);
    return catalog.entries.filter((e) => on.has(e.id));
  }, [catalog]);
  const hidden = catalog.entries.length - enabled.length;

  // Categories in catalog order — the order the sections appear in.
  const categories = useMemo(() => [...new Set(enabled.map((e) => e.category))], [enabled]);

  const q = search.trim().toLowerCase();
  const shown = enabled.filter(
    (e) =>
      (category === ALL || e.category === category) &&
      (!q ||
        e.title.toLowerCase().includes(q) ||
        e.description.toLowerCase().includes(q) ||
        (e.highlights ?? []).some((h) => h.toLowerCase().includes(q))),
  );
  const grouped = new Map<string, CatalogEntry[]>();
  for (const e of shown) grouped.set(e.category, [...(grouped.get(e.category) ?? []), e]);

  // The featured row: the first few apps with pictures, when nothing has
  // narrowed the page. A search or a category is the operator's own
  // shortlist, and a row of unrelated apps above it is in the way.
  const featured = !q && category === ALL ? enabled.filter((e) => e.screenshots?.length).slice(0, 3) : [];

  return (
    <div className="space-y-10">
      <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
        <input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search apps…"
          aria-label="Search the app store"
          className={`${control} w-full max-w-xs`}
        />
        {categories.length > 1 && (
          <Tabs
            variant="pills"
            label="Category"
            active={category}
            onChange={setCategory}
            items={[{ id: ALL, label: "All" }, ...categories.map((c) => ({ id: c, label: c }))]}
          />
        )}
      </div>

      {capacity.data && <Machine cap={capacity.data} />}

      {enabled.length === 0 && (
        <p className="text-sm text-ink-muted">
          No apps are enabled yet. Choose which ones appear in{" "}
          <SettingsLink to="preferences/app-store" className="text-accent hover:underline" />.
        </p>
      )}
      {enabled.length > 0 && shown.length === 0 && (
        <p className="text-sm text-ink-muted">
          No apps match “{search.trim()}”{category !== ALL ? ` under ${category}` : ""}.
          {hidden > 0 && <> {hidden} more {hidden === 1 ? "is" : "are"} hidden — see <SettingsLink to="preferences/app-store" />.</>}
        </p>
      )}

      {featured.length > 0 && (
        <section>
          <SectionHead title="Featured" />
          <div className="mt-4 grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
            {featured.map((e) => (
              <Link
                key={e.id}
                to={paths.storeApp(e.id)}
                className="group overflow-hidden rounded-lg border border-veil-edge bg-veil transition-colors hover:border-edge-hover"
              >
                <div className="aspect-[16/9] overflow-hidden border-b border-hair bg-code">
                  <img
                    src={e.screenshots![0].src}
                    alt={e.screenshots![0].alt}
                    loading="lazy"
                    className="h-full w-full object-cover object-top"
                  />
                </div>
                <div className="flex items-center gap-3 p-3.5">
                  <AppIcon entry={e} size={44} />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-sm font-medium text-ink">{e.title}</span>
                    <span className="block truncate text-hint text-ink-muted">{e.description}</span>
                  </span>
                  <Chip entry={e} installed={installed.has(e.id)} />
                </div>
              </Link>
            ))}
          </div>
        </section>
      )}

      {[...grouped.entries()].map(([cat, list]) => (
        <section key={cat}>
          <SectionHead title={cat} right={<span className="text-hint text-ink-muted">{list.length} {list.length === 1 ? "app" : "apps"}</span>} />
          <div className="mt-4 grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {list.map((e) => (
              <Link
                key={e.id}
                to={paths.storeApp(e.id)}
                className="flex items-center gap-3.5 rounded-lg border border-veil-edge bg-veil p-3.5 transition-colors hover:border-edge-hover"
              >
                <AppIcon entry={e} size={56} />
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm font-medium text-ink">{e.title}</span>
                  <span className="mt-0.5 line-clamp-2 text-hint leading-snug text-ink-muted">{e.description}</span>
                  {/* Already here: what it would add is moot, so no verdict. */}
                  <Cost entry={e} fit={installed.has(e.id) ? undefined : fits[e.id]} />
                </span>
                <Chip entry={e} installed={installed.has(e.id)} />
              </Link>
            ))}
          </div>
        </section>
      ))}

      {hidden > 0 && shown.length > 0 && (
        <p className="text-hint text-ink-muted">
          {hidden} more {hidden === 1 ? "app is" : "apps are"} not shown. Choose which appear in{" "}
          <SettingsLink to="preferences/app-store" className="text-ink-muted hover:text-ink hover:underline" />.
        </p>
      )}
    </div>
  );
}

// What the machine has and is carrying, at the top of the store: the
// memory bar with the installed apps on it, the cores, the free disk. The
// number a person needs while browsing is "how much room is left", and
// it is here rather than on every card.
function Machine({ cap }: { cap: Capacity }) {
  const { host, apps } = cap;
  const appsMem = apps.reduce((n, a) => n + a.memUsage, 0);
  const other = Math.max(0, host.memUsed - appsMem);
  const running = apps.filter((a) => a.running).length;
  const facts = [
    host.memTotal > 0 ? `${approx(host.memUsed)} of ${approx(host.memTotal)} memory in use` : "memory not measured yet",
    host.cores > 0 ? `${host.cores} ${host.cores === 1 ? "core" : "cores"}` : "",
    host.diskTotal > 0 ? `${approx(host.diskFree)} free of ${approx(host.diskTotal)} on the data disk` : "",
  ].filter(Boolean);
  return (
    <section>
      <SectionHead
        title="This machine"
        right={
          <span className="text-hint text-ink-muted">
            {apps.length === 0 ? "nothing installed yet" : `${running} of ${apps.length} ${apps.length === 1 ? "app" : "apps"} running`}
          </span>
        }
      />
      <div className="mt-4 grid gap-x-10 gap-y-3 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
        <p className="text-sm text-ink-muted">{facts.join(" · ")}</p>
        {host.memTotal > 0 && (
          <CapacityBar
            total={host.memTotal}
            bands={[
              { label: apps.length === 1 ? "1 app" : `${apps.length} apps`, bytes: appsMem, kind: "apps" },
              { label: "System", bytes: other, kind: "other" },
            ]}
          />
        )}
      </div>
    </section>
  );
}

// One line of cost under a card's description, amber when it would not
// fit plainly.
function Cost({ entry, fit }: { entry: CatalogEntry; fit: Capacity["fits"][string] | undefined }) {
  const line = cardLine(entry, fit);
  if (!line) return null;
  return <span className={`mt-1 block truncate text-[11px] ${line.tone === "ok" ? "text-ink-muted" : TONE_TEXT[line.tone]}`}>{line.text}</span>;
}

// What pressing the card gets you. The chip is part of the card, not a
// second button: the card is one link, and this is its answer.
function Chip({ entry, installed }: { entry: CatalogEntry; installed: boolean }) {
  if (installed) {
    return (
      <span className="shrink-0 rounded-full border border-veil-edge bg-hover px-2.5 py-0.5 text-xs font-medium text-ink-muted">
        Installed
      </span>
    );
  }
  if (entry.blocker) {
    return (
      <span className="shrink-0 rounded-full border border-warn/30 bg-warn/10 px-2.5 py-0.5 text-xs font-medium text-warn" title={entry.blocker}>
        Can't run here
      </span>
    );
  }
  return (
    <span className={buttonClass("secondary", "sm")}>
      Install
    </span>
  );
}
