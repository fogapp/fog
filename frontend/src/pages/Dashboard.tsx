import { useMemo, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { post, type AppStatus } from "../api";
import { LoadError, Loading, Spinner, StaleNote, errorText, phaseOf } from "../components/Async";
import Button from "../components/Button";
import { IconButton, OpenGlyph, PlayGlyph, StopGlyph } from "./app/Actions";
import { SectionHead } from "../components/Section";
import { useDismissable } from "../lib/useDismissable";
import { useFlash } from "../lib/useFlash";
import MetricsRibbon from "../components/MetricsRibbon";
import { appsKey, useApps } from "../lib/useApps";
import { paths } from "../lib/paths";

// Home is the host and every app. What needs a human — errors, issues,
// notices, a fog release — is the bell's job in the header, and saying it
// twice only splits the one attention channel that exists; environment
// warnings and a missing Docker are issues now, so they arrive there too.
// What stays here is what belongs to a row: an app's update pill, its open
// error count, a staged restore.
export default function Dashboard() {
  return (
    // The ribbon sits flush under the header by design; the padding at the
    // bottom is only so the last row isn't jammed against the window.
    <div className="space-y-6 pb-6">
      <MetricsRibbon />
      <AppsTable />
    </div>
  );
}

// Apps as a control surface, not a gallery: every row can be started, stopped,
// updated, or backed up right here — the app page is for digging in, and a
// click on the row goes there.

type SortKey = "name" | "state" | "image" | "cpu" | "mem";

// The table is the page, so it runs to the window's edges like the ribbon
// above it — <main> adds no padding — and the outer cells carry the page
// gutter themselves so the columns still line up with everything else.
// Keep these in step with Layout's PAGE_PAD.
const GUTTER_L = "pl-4 sm:pl-6 lg:pl-8";
const GUTTER_R = "pr-4 sm:pr-6 lg:pr-8";

// Running first, then deploying, trouble, stopped, and not-yet-real states.
function stateRank(a: AppStatus): number {
  if (a.deploying) return 1;
  if (a.state === "running") return a.healthy === false ? 2 : 0;
  if (a.state === "new" || a.state === "missing") return 4;
  return 3;
}

function AppsTable() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [note, flash] = useFlash();
  const [sort, setSort] = useState<{ key: SortKey; dir: 1 | -1 } | null>(null);

  const apps = useApps({ stats: true, refetchInterval: 5000 });
  const data = apps.data;

  const rows = useMemo(() => {
    const out = [...(data ?? [])];
    if (!sort) return out;
    const { key, dir } = sort;
    out.sort((a, b) => {
      switch (key) {
        case "name":
          return dir * a.name.localeCompare(b.name);
        case "state":
          return dir * (stateRank(a) - stateRank(b) || a.name.localeCompare(b.name));
        case "image":
          return dir * (a.image ?? "").localeCompare(b.image ?? "");
        // Unmeasured rows sort as zero rather than jumping around: they carry
        // no figure to rank, and NaN comparisons would scramble the order.
        case "cpu":
          return dir * ((a.cpuPercent ?? 0) - (b.cpuPercent ?? 0));
        case "mem":
          return dir * ((a.memUsage ?? 0) - (b.memUsage ?? 0));
      }
    });
    return out;
  }, [data, sort]);

  const toggleSort = (key: SortKey) =>
    setSort((s) => (s?.key === key ? { key, dir: s.dir === 1 ? -1 : 1 } : { key, dir: 1 }));

  // Awaited so a button stops saying "…" only once the row behind it has
  // actually been refetched — never a beat before. The whole ["apps"] prefix,
  // not just this table's stats view: the sidebar list reads the same fact,
  // and a stopped app must lose its green dot everywhere at once.
  const refresh = () => qc.invalidateQueries({ queryKey: appsKey });

  const action = useMutation({
    mutationFn: ({ name, action }: { name: string; action: string }) =>
      post(`/api/apps/${name}/actions`, { action }),
    onSuccess: refresh,
    onError: (e) => flash(errorText(e, "Action failed")),
  });
  const update = useMutation({
    mutationFn: (name: string) => post(`/api/apps/${name}/update`),
    onSuccess: refresh,
    onError: (e) => flash(errorText(e, "Update failed")),
  });
  const backup = useMutation({
    mutationFn: (name: string) => post(`/api/apps/${name}/backups`),
    onSuccess: (_d, name) => flash(`Backed up ${name} ✓`),
    onError: (e) => flash(errorText(e, "Backup failed")),
  });

  const phase = phaseOf(apps, data?.length);

  return (
    <div className="min-w-0">
      {/* No rule under the title: the column header row carries one right
          below it, and two hairlines a line apart read as a mistake. */}
      <h2 className={`m-0 pb-3 ${GUTTER_L} ${GUTTER_R}`}>
        <SectionHead
          title="Apps"
          right={
            <>
              {/* The kebab menu closes on any click, so a pending label inside it
                  would never be seen — progress reports here, where the success
                  flash already lands. */}
              {backup.isPending ? (
                <span className="text-xs text-ink-muted">Backing up {backup.variables}…</span>
              ) : (
                note && <span className="text-xs text-ink-muted">{note}</span>
              )}
              <Link
                to={paths.newApp}
                className="rounded-md bg-accent px-2.5 py-1 text-xs font-medium text-accent-on hover:bg-accent-hover"
              >
                Add app
              </Link>
            </>
          }
        />
      </h2>

      {phase === "loading" ? (
        <div className={`py-8 text-center ${GUTTER_L} ${GUTTER_R}`}>
          <Loading label="Loading your apps…" className="justify-center" />
        </div>
      ) : phase === "error" ? (
        <div className={`py-4 ${GUTTER_L} ${GUTTER_R}`}>
          <LoadError label="Couldn't load your apps." error={apps.error} onRetry={() => apps.refetch()} />
        </div>
      ) : phase === "empty" ? (
        <div className={`py-8 text-center ${GUTTER_L} ${GUTTER_R}`}>
          <p className="text-ink-muted">No apps yet.</p>
          <p className="mt-1 text-sm text-ink-muted">Add your first app — a container image is all you need.</p>
        </div>
      ) : (
        <table className="w-full text-left text-cell">
          <thead>
            <tr className="caps-label border-b border-hair-strong">
              <Th label="App" k="name" sort={sort} onSort={toggleSort} className={`py-2.5 pr-3 ${GUTTER_L}`} />
              <Th label="Status" k="state" sort={sort} onSort={toggleSort} className="px-3 py-2.5" />
              <Th label="Image" k="image" sort={sort} onSort={toggleSort} className="hidden px-3 py-2.5 md:table-cell" />
              <Th label="CPU" k="cpu" sort={sort} onSort={toggleSort} right className="px-3 py-2.5" />
              <Th label="Mem" k="mem" sort={sort} onSort={toggleSort} right className="px-3 py-2.5" />
              <th className={`py-2.5 pl-3 ${GUTTER_R}`} />
            </tr>
          </thead>
          <tbody>
            {rows.map((a) => {
              const busy =
                (action.isPending && action.variables?.name === a.name) ||
                (update.isPending && update.variables === a.name);
              const running = a.state === "running";
              const host = a.hosts?.[0];
              const openURL = host
                ? `https://${host}`
                : a.hostPort > 0
                  ? `http://${window.location.hostname}:${a.hostPort}`
                  : null;
              return (
                <tr
                  key={a.name}
                  onClick={() => navigate(paths.app(a.name))}
                  className="cursor-pointer border-b border-divider last:border-0 hover:bg-hover"
                >
                  <td className={`py-2.5 pr-3 ${GUTTER_L}`}>
                    <Link to={paths.app(a.name)} onClick={(e) => e.stopPropagation()} className="font-medium text-ink hover:underline">
                      {a.name}
                    </Link>
                    {a.updateAvailable && (
                      <span className="ml-2 rounded-full bg-accent/10 px-2 py-px text-[11px] font-medium text-accent">
                        Update
                      </span>
                    )}
                    {(a.openErrors ?? 0) > 0 && (
                      <Link
                        to={paths.app(a.name, "errors")}
                        onClick={(e) => e.stopPropagation()}
                        className="ml-2 rounded-full bg-warn/10 px-1.5 py-0.5 text-[10px] font-medium text-warn hover:bg-warn/10"
                      >
                        {a.openErrors} error{a.openErrors === 1 ? "" : "s"}
                      </Link>
                    )}
                    {a.restorePending && (
                      <span className="ml-2 rounded-full bg-warn/10 px-1.5 py-0.5 text-[10px] font-medium text-warn">
                        restore staged
                      </span>
                    )}
                  </td>
                  <td className="px-3 py-2.5">
                    {a.deploying ? (
                      <span className="flex items-center gap-1.5 text-xs font-medium text-accent">
                        <span className="inline-block h-2.5 w-2.5 animate-spin rounded-full border border-accent/30 border-t-transparent" />
                        deploying
                      </span>
                    ) : (
                      <StateLabel state={a.state} healthy={a.healthy} />
                    )}
                  </td>
                  <td className="hidden max-w-48 truncate px-3 py-2.5 text-xs text-ink-muted md:table-cell">
                    {a.image || "—"}
                  </td>
                  {/* A running app with no reading yet is "…", not 0% — the
                      sampler simply hasn't got to it. */}
                  <td className="px-3 py-2.5 text-right tabular-nums text-ink-muted">
                    {!running ? "—" : a.cpuPercent === undefined ? "…" : `${a.cpuPercent.toFixed(1)}%`}
                  </td>
                  <td className="px-3 py-2.5 text-right tabular-nums text-ink-muted">
                    {!running ? "—" : a.memUsage === undefined ? "…" : `${(a.memUsage / 1024 / 1024).toFixed(0)} MB`}
                  </td>
                  <td className={`py-2.5 pl-3 ${GUTTER_R}`} onClick={(e) => e.stopPropagation()}>
                    <div className="flex items-center justify-end gap-1">
                      {a.updateAvailable && !a.deploying && (
                        <Button size="sm" variant="info" onClick={() => update.mutate(a.name)} disabled={busy}>
                          Update
                        </Button>
                      )}
                      {/* The app page's pair: open it, and stop or start it.
                          Both cells stay put whatever the state, so the
                          column never shifts; a cell that can't act says why. */}
                      <span className="inline-flex divide-x divide-hair rounded-md border border-edge">
                        <IconButton
                          label={`Open ${a.name}`}
                          title={openURL && running ? "Open app" : running ? "No web address" : "Not running"}
                          href={openURL && running ? openURL : undefined}
                          disabled={!openURL || !running}
                          tone="accent"
                          edge="start"
                        >
                          <OpenGlyph />
                        </IconButton>
                        <IconButton
                          label={`${running ? "Stop" : "Start"} ${a.name}`}
                          title={a.deploying ? "Deploying…" : a.state === "new" ? "Not deployed yet" : running ? "Stop" : "Start"}
                          onClick={() => action.mutate({ name: a.name, action: running ? "stop" : "start" })}
                          disabled={busy || a.deploying || a.state === "new"}
                          edge="end"
                        >
                          {busy ? <Spinner size="h-3.5 w-3.5" tone="border-fog-300" /> : running ? <StopGlyph /> : <PlayGlyph />}
                        </IconButton>
                      </span>
                      <Kebab>
                        {running && !a.deploying && (
                          <MenuItem onClick={() => action.mutate({ name: a.name, action: "restart" })}>
                            Restart
                          </MenuItem>
                        )}
                        {!a.deploying && a.state !== "new" && (
                          <MenuItem onClick={() => action.mutate({ name: a.name, action: "deploy" })}>
                            Redeploy
                          </MenuItem>
                        )}
                        {a.state !== "new" && (
                          <MenuItem onClick={() => backup.mutate(a.name)} disabled={backup.isPending}>
                            Back up now
                          </MenuItem>
                        )}
                        <MenuItem onClick={() => navigate(paths.app(a.name, "logs"))}>Logs</MenuItem>
                      </Kebab>
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      {apps.isError && data && (
        <div className={`pt-3 ${GUTTER_L} ${GUTTER_R}`}>
          <StaleNote what="these states and figures" />
        </div>
      )}
    </div>
  );
}

function Kebab({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useDismissable(ref, open, () => setOpen(false));
  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => setOpen(!open)}
        aria-label="More actions"
        className="rounded-md px-1.5 py-1 text-ink-muted hover:bg-fog-800 hover:text-ink"
      >
        ⋯
      </button>
      {open && (
        <div
          onClick={() => setOpen(false)}
          className="absolute right-0 z-20 mt-1 w-44 overflow-hidden rounded-lg border border-veil-edge bg-fog-900 py-1 shadow-xl shadow-black/40"
        >
          {children}
        </div>
      )}
    </div>
  );
}

function MenuItem({ onClick, disabled, children }: {
  onClick: () => void;
  disabled?: boolean;
  children: React.ReactNode;
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      className="block w-full px-3 py-1.5 text-left text-sm text-ink hover:bg-fog-800 hover:text-ink disabled:cursor-not-allowed disabled:opacity-40"
    >
      {children}
    </button>
  );
}

// Status as icon + word, not a bare dot — readable at a glance and it doesn't
// rely on color alone.
function StateLabel({ state, healthy }: { state: string; healthy?: boolean }) {
  const [cls, icon, label] =
    state === "running"
      ? healthy === false
        ? ["text-warn", "⚠", "Unhealthy"]
        : ["text-accent", "✓", "Running"]
      : state === "new"
        ? ["text-ink-muted", "○", "Needs setup"]
        : state === "missing"
          ? ["text-ink-muted", "○", "Missing"]
          : ["text-ink-muted", "○", state ? state[0].toUpperCase() + state.slice(1) : state];
  return (
    <span className={`flex shrink-0 items-center gap-1 text-xs font-medium ${cls}`}>
      <span aria-hidden>{icon}</span>
      {label}
    </span>
  );
}

// A sortable header cell: click cycles ascending → descending. The arrow is
// the only state indicator, so it rides right next to the label.
function Th({ label, k, sort, onSort, right, className }: {
  label: string;
  k: SortKey;
  sort: { key: SortKey; dir: 1 | -1 } | null;
  onSort: (k: SortKey) => void;
  right?: boolean;
  className?: string;
}) {
  const active = sort?.key === k;
  return (
    <th className={`font-medium ${right ? "text-right" : "text-left"} ${className ?? ""}`}>
      <button
        onClick={() => onSort(k)}
        className={`inline-flex items-center gap-1 hover:text-ink ${active ? "text-ink" : ""}`}
        aria-label={`Sort by ${label}`}
      >
        {label}
        {active && <span aria-hidden>{sort!.dir === 1 ? "▲" : "▼"}</span>}
      </button>
    </th>
  );
}
