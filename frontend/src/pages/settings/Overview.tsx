// Settings › Overview: the page the Settings row opens. Before any one
// control, two answers: does anything here need you, and is everything
// fog runs on there? The first is the open issues whose fix is a Settings
// page; the second is one card per area with each thing it manages, its
// state, and the way in.

import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { get, type IssuesResponse, type OffsiteTargetsResponse, type PeopleDirectory } from "../../api";
import { ButtonLink } from "../../components/Button";
import PageHeader from "../../components/PageHeader";
import { Section, StatusLine } from "../../components/Section";
import { RowItem, RowList } from "../../components/SettingRow";
import { formatBytes } from "../../lib/format";
import { paths } from "../../lib/paths";
import { SETTINGS_AREAS, type SettingsAreaId, type SettingsTo } from "../../lib/settings";
import { Dot } from "../app/cockpit";
import { useDependencies } from "./ContainerFacts";
import { useDomainStatus } from "./Domain";
import { offsiteKey } from "./Offsite";
import { backupsStatusQuery, mcpTokensQuery, settingsQuery, systemQuery, tunnelQuery, vpnQuery } from "../../queries";

export default function Overview() {
  const rows = useRows();
  return (
    <div className="max-w-5xl space-y-10">
      <PageHeader size="md" title="Settings" description="Everything this server runs on, and the controls for each of it." />
      <AttentionSection rows={rows} />
      <div className="grid gap-4 lg:grid-cols-2">
        {SETTINGS_AREAS.map((a) => (
          <AreaCard key={a.id} area={a} rows={inPageOrder(a, rows.filter((r) => r.area === a.id))} />
        ))}
      </div>
    </div>
  );
}

// A card lists its rows in the order its pages are listed, so the card
// and the submenu read the same way down.
function inPageOrder(area: (typeof SETTINGS_AREAS)[number], rows: Row[]): Row[] {
  const at = (r: Row) => area.pages.findIndex((p) => r.to.split("#")[0] === paths.settings(`${area.id}/${p.id}` as SettingsTo));
  return [...rows].sort((a, b) => at(a) - at(b));
}

type Tone = "ok" | "warn" | "idle" | "unknown";

interface Row {
  area: SettingsAreaId;
  name: string;
  role: string;
  tone: Tone;
  status: string;
  // The Settings page that manages it.
  to: string;
}

// Every dependency as one row: what it does for fog, whether it is up, and
// where to go. "unknown" is a state of its own — before a status arrives,
// nothing here claims anything.
function useRows(): Row[] {
  const system = useQuery({ ...systemQuery, refetchInterval: 30_000 });
  const deps = useDependencies();
  const domain = useDomainStatus();
  const tunnel = useQuery({ ...tunnelQuery, refetchInterval: 15_000 });
  const vpn = useQuery({ ...vpnQuery, refetchInterval: 15_000 });
  const settings = useQuery(settingsQuery);
  const offsite = useQuery({ queryKey: offsiteKey, queryFn: () => get<OffsiteTargetsResponse>("/api/offsite/targets") });
  const backups = useQuery({ ...backupsStatusQuery, refetchInterval: 60_000 });
  const people = useQuery({ queryKey: ["people"], queryFn: () => get<PeopleDirectory>("/api/people") });
  const tokens = useQuery(mcpTokensQuery);

  const rows: Row[] = [];

  {
    const d = system.data?.docker;
    rows.push({
      area: "server", name: "Docker", role: "Runs every app",
      tone: !system.data ? "unknown" : d?.available ? "ok" : "warn",
      status: !system.data
        ? system.isError ? "status unavailable" : "checking…"
        : d?.available
          ? `${d.version?.Version ?? "running"}${d.info ? ` · ${d.info.ContainersRunning} of ${d.info.Containers} containers running` : ""}`
          : "not reachable",
      to: paths.settings("server/docker"),
    });
  }
  {
    const f = deps.data?.postgres;
    rows.push({
      area: "storage", name: "Postgres", role: "The shared database",
      tone: !deps.data ? "unknown" : !deps.data.docker.available ? "warn" : !f?.found ? "idle" : f.running ? "ok" : "warn",
      status: !deps.data
        ? deps.isError ? "status unavailable" : "checking…"
        : !deps.data.docker.available ? "needs Docker"
          : !f?.found ? "no cluster yet"
            : f.running ? `running${f.memUsage !== undefined ? ` · ${formatBytes(f.memUsage)}` : ""}` : "not running",
      to: paths.settings("storage/database"),
    });
  }
  {
    const st = domain.data;
    const f = deps.data?.proxy;
    rows.push({
      area: "network", name: "Proxy", role: "Routes hostnames to apps",
      tone: !st ? "unknown" : st.proxyRunning ? "ok" : st.domain ? "warn" : "idle",
      status: !st
        ? domain.isError ? "status unavailable" : "checking…"
        : st.proxyRunning ? `running${f?.memUsage !== undefined ? ` · ${formatBytes(f.memUsage)}` : ""}`
          : st.domain ? "not running" : "no domain set up",
      to: paths.settings("network/proxy"),
    });
  }
  {
    const st = tunnel.data;
    const failing = !!st && !st.running && !!(st.lastError || st.installError);
    rows.push({
      area: "network", name: "Cloudflare Tunnel", role: "Reach from outside without opening ports",
      tone: !st ? "unknown" : st.running ? "ok" : failing ? "warn" : "idle",
      status: !st
        ? tunnel.isError ? "status unavailable" : "checking…"
        : st.running ? `running${st.restarts ? ` · ${st.restarts} restarts` : ""}`
          : failing ? "failing" : st.tokenConfigured ? "stopped" : "not set up",
      to: paths.settings("network/tunnel"),
    });
  }
  {
    const st = vpn.data;
    const failing = !!st && !st.running && !!(st.lastError || st.installError || st.upError);
    rows.push({
      area: "network", name: "Tailscale", role: "Away access over your tailnet",
      tone: !st ? "unknown" : st.running && st.loggedIn ? "ok" : failing ? "warn" : "idle",
      status: !st
        ? vpn.isError ? "status unavailable" : "checking…"
        : st.running && st.loggedIn ? `connected${st.version ? ` · ${st.version}` : ""}`
          : st.running ? "running, not signed in" : failing ? "failing" : st.keyConfigured ? "stopped" : "not set up",
      to: paths.settings("network/away"),
    });
  }
  {
    const s = settings.data;
    const server = s?.ntfy_server?.trim() ?? "";
    rows.push({
      area: "preferences", name: "ntfy", role: "Push notifications",
      tone: !s ? "unknown" : server ? "ok" : "idle",
      status: !s
        ? settings.isError ? "status unavailable" : "checking…"
        : server ? `${server.replace(/^https?:\/\//, "")}${s.ntfy_topic ? ` · ${s.ntfy_topic}` : ""}` : "not set up",
      to: paths.settings("preferences/notifications", "ntfy"),
    });
  }
  {
    const targets = offsite.data?.targets;
    const active = (targets ?? []).filter((t) => t.role === "active");
    const worst = active.find((t) => t.lastError);
    rows.push({
      area: "storage", name: "Offsite locker", role: "Backups off this machine",
      tone: !targets ? "unknown" : active.length === 0 ? "idle" : worst ? "warn" : "ok",
      status: !targets
        ? offsite.isError ? "status unavailable" : "checking…"
        : active.length === 0 ? "none set up"
          : active.map((t) => `${t.name}${t.grade ? ` · grade ${t.grade}` : ""}`).join(", ") + (worst ? " · last sync failed" : ""),
      to: paths.settings("storage/offsite"),
    });
  }
  {
    const s = settings.data;
    const src = s?.update_base_url?.trim() ?? "";
    rows.push({
      area: "server", name: "Release repository", role: "Where fog's own updates come from",
      tone: !s ? "unknown" : "ok",
      status: !s ? (settings.isError ? "status unavailable" : "checking…") : src ? src.replace(/^https?:\/\//, "") : "the default repository",
      to: paths.settings("server/updates", "fog"),
    });
  }
  {
    const st = domain.data;
    rows.push({
      area: "network", name: "Domain & HTTPS", role: "Clean addresses with real certificates",
      tone: !st ? "unknown" : !st.domain ? "idle" : st.httpsOk === false ? "warn" : "ok",
      status: !st ? (domain.isError ? "status unavailable" : "checking…") : st.domain || "LAN addresses only",
      to: paths.settings("network/domain"),
    });
  }
  {
    const b = backups.data;
    const stale = (b?.apps.filter((a) => a.stale).length ?? 0) + (b?.files?.stale ? 1 : 0);
    const off = b?.apps.filter((a) => !a.enabled).length ?? 0;
    rows.push({
      area: "storage", name: "Backups", role: "Nightly snapshots of every app",
      tone: !b ? "unknown" : stale > 0 ? "warn" : "ok",
      status: !b
        ? backups.isError ? "status unavailable" : "checking…"
        : stale > 0 ? `${stale} overdue` : off > 0 ? `current · ${off} not backed up` : "everything current",
      to: paths.settings("storage/backups"),
    });
  }
  {
    const p = people.data;
    const n = p?.people.length ?? 0;
    rows.push({
      area: "access", name: "People", role: "Accounts on this fog",
      tone: !p ? "unknown" : "ok",
      status: !p
        ? people.isError ? "status unavailable" : "checking…"
        : `${n} ${n === 1 ? "account" : "accounts"}${p.invites.length ? ` · ${p.invites.length} open invite${p.invites.length === 1 ? "" : "s"}` : ""}`,
      to: paths.settings("access/people"),
    });
  }
  {
    const t = tokens.data?.tokens;
    rows.push({
      area: "access", name: "AI assistants", role: "MCP clients that can act here",
      tone: !t ? "unknown" : t.length ? "ok" : "idle",
      status: !t ? (tokens.isError ? "status unavailable" : "checking…") : t.length ? `${t.length} client${t.length === 1 ? "" : "s"}` : "off",
      to: paths.settings("access/ai"),
    });
  }
  {
    const s = settings.data;
    rows.push({
      area: "preferences", name: "Interface", role: "How much of fog the household sees",
      tone: !s ? "unknown" : "ok",
      status: !s ? (settings.isError ? "status unavailable" : "checking…") : s.ui_mode === "basic" ? "basic" : "advanced",
      to: paths.settings("preferences/appearance", "interface"),
    });
  }

  return rows;
}

// What needs you, in Settings terms: every open issue whose fix is a
// Settings page, and anything on the cards below that is amber. Quiet when
// there is nothing — a page that always shouts is a page nobody reads.
function AttentionSection({ rows }: { rows: Row[] }) {
  const issues = useQuery({
    queryKey: ["issues", "open"],
    queryFn: () => get<IssuesResponse>("/api/issues?status=open"),
    refetchInterval: 15_000,
  });
  const fixes = (issues.data?.issues ?? []).filter((i) => i.fixUrl?.startsWith("/settings"));
  // An amber card row that an issue already speaks for would say it twice.
  const covered = new Set(fixes.map((i) => i.fixUrl!.split("#")[0]));
  const warn = rows.filter((r) => r.tone === "warn" && !covered.has(r.to.split("#")[0]));
  const known = !!issues.data && rows.every((r) => r.tone !== "unknown");

  if (fixes.length === 0 && warn.length === 0) {
    return known ? <StatusLine tone="ok">Nothing in Settings needs you.</StatusLine> : null;
  }
  return (
    <Section id="attention" title="Needs attention" actions={<span className="text-hint text-ink-muted">{fixes.length + warn.length}</span>}>
      <RowList>
        {fixes.map((i) => (
          <RowItem
            key={`issue-${i.id}`}
            title={<span className="flex items-center gap-2"><Dot tone={i.severity === "info" ? "off" : "warn"} />{i.title}</span>}
            meta={i.fix || i.detail}
            actions={<ButtonLink size="sm" to={i.fixUrl!}>Fix</ButtonLink>}
          />
        ))}
        {warn.map((r) => (
          <RowItem
            key={`row-${r.name}`}
            title={<span className="flex items-center gap-2"><Dot tone="warn" />{r.name}: {r.status}</span>}
            meta={r.role}
            actions={<ButtonLink size="sm" to={r.to}>Open</ButtonLink>}
          />
        ))}
      </RowList>
    </Section>
  );
}

// One area: its name (the way in), each thing it manages with its state,
// and its pages as links along the foot.
function AreaCard({ area, rows }: { area: (typeof SETTINGS_AREAS)[number]; rows: Row[] }) {
  return (
    <section className="flex flex-col rounded-lg border border-veil-edge bg-veil">
      <Link
        to={paths.settings(area.id)}
        className="group flex items-center justify-between rounded-t-lg px-[18px] pb-2 pt-4 hover:bg-hover"
      >
        <h2 className="text-[17px] font-medium text-ink">{area.label}</h2>
        <span aria-hidden className="text-ink-faint transition-colors group-hover:text-ink">›</span>
      </Link>
      <ul className="flex-1 px-[18px]">
        {rows.map((r) => (
          <li key={r.name} className="border-b border-divider/60 last:border-0">
            <Link to={r.to} className="group flex items-center gap-3 py-2.5" title={r.role}>
              <Dot tone={r.tone === "ok" ? "ok" : r.tone === "warn" ? "warn" : "off"} />
              <span className="text-cell text-ink group-hover:underline">{r.name}</span>
              <span
                className={`ml-auto min-w-0 truncate text-right text-hint ${
                  r.tone === "warn" ? "text-warn" : r.tone === "ok" ? "text-ink-muted" : "text-ink-muted"
                }`}
              >
                {r.status}
              </span>
            </Link>
          </li>
        ))}
      </ul>
      <nav aria-label={`${area.label} pages`} className="flex flex-wrap gap-x-3 gap-y-1 border-t border-hair px-[18px] py-3 text-hint">
        {area.pages.map((p) => (
          <Link key={p.id} to={paths.settings(`${area.id}/${p.id}` as SettingsTo)} className="text-ink-muted hover:text-ink">
            {p.label}
          </Link>
        ))}
      </nav>
    </section>
  );
}
