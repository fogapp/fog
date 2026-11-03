// Basic mode: the whole app surface collapses to a splash page — the app's
// identity and the four verbs a non-operator needs: stop, restart, update,
// open. Everything else comes back from the avatar menu → Switch to advanced mode.

import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";
import { ApiError, post, type UpdateCheckResponse } from "../api";
import { Loading, LoadError, Spinner, errorText } from "../components/Async";
import AppIcon from "../components/AppIcon";
import StateBadge from "../components/StateBadge";
import { StateDot } from "../components/StateBadge";
import { routeHost } from "../lib/hostname";
import { useApps } from "../lib/useApps";
import { useAppDetail } from "../lib/useAppData";
import { useFlash } from "../lib/useFlash";
import { paths } from "../lib/paths";
import { catalogQuery, settingsQuery } from "../queries";

// The basic-mode dashboard: every installed app as a tile, filling the page
// width, plus the one growth action — install more. Same query key as the
// sidebar, so the two lists can never disagree.
export function BasicHome() {
  const apps = useApps({ refetchInterval: 15000 });
  const catalog = useQuery(catalogQuery);
  const entryFor = (name: string) => catalog.data?.entries.find((e) => e.id === name);

  const installButton = (
    <Link to={paths.newApp}
      className="shrink-0 rounded-md bg-accent px-4 py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover">
      + Install apps
    </Link>
  );

  if (apps.isLoading) return <Loading label="Loading apps…" className="justify-center" />;
  if (apps.isError) return <LoadError label="Couldn't load your apps." error={apps.error} onRetry={() => apps.refetch()} />;

  const list = apps.data ?? [];
  if (list.length === 0) {
    return (
      <div className="flex flex-col items-center gap-4 py-16 text-center">
        <p className="text-ink-muted">Nothing installed yet.</p>
        {installButton}
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold text-ink">Apps</h1>
        {installButton}
      </div>
      <div className="grid grid-cols-[repeat(auto-fill,minmax(12rem,1fr))] gap-4">
        {list.map((a) => {
          const entry = entryFor(a.name);
          return (
            <Link key={a.name} to={paths.app(a.name)}
              className="flex flex-col items-center gap-3 rounded-lg border border-veil-edge bg-veil p-6 text-center transition-colors hover:border-fog-600 hover:bg-hover">
              <AppIcon entry={entry} size={56} />
              <span className="w-full space-y-1">
                <span className="flex items-center justify-center gap-2 text-sm font-medium text-ink">
                  <StateDot app={a} />
                  <span className="truncate">{entry?.title ?? a.name}</span>
                </span>
                {entry?.description && (
                  <span className="block truncate text-xs text-ink-muted">{entry.description}</span>
                )}
              </span>
            </Link>
          );
        })}
      </div>
    </div>
  );
}

export default function BasicApp() {
  const { name = "" } = useParams();
  const [error, setError] = useState("");
  // "Up to date ✓" briefly replaces the Update label after a clean check.
  const [upToDateNote, flashUpToDate] = useFlash();

  // The same shell state as the advanced page, so a deploy that finishes
  // while this page is open flips it the same way.
  const { detail, deployStatus, refresh } = useAppDetail(name);
  const statuses = useApps({ stats: true, refetchInterval: 5000 });
  const settings = useQuery(settingsQuery);
  const catalog = useQuery(catalogQuery);

  const action = useMutation({
    mutationFn: (a: string) => post(`/api/apps/${name}/actions`, { action: a }),
    onSuccess: () => {
      setError("");
      return refresh();
    },
    onError: (e) => setError(errorText(e, "That didn't work")),
  });

  // One button does the whole job: check, and if something newer exists,
  // install it. "Up to date ✓" is only said after the check came back clean.
  const update = useMutation({
    mutationFn: async () => {
      const status = statuses.data?.find((s) => s.name === name);
      if (!status?.updateAvailable) {
        const res = await post<UpdateCheckResponse>(`/api/updates/check?app=${encodeURIComponent(name)}`);
        const r = res.results[0];
        if (r?.error) throw new Error(r.error);
        if (r?.deploying) return "busy";
        if (!r?.updateAvailable) return "current";
      }
      await post(`/api/apps/${name}/update`);
      return "updating";
    },
    onSuccess: async (outcome) => {
      setError("");
      await refresh();
      if (outcome === "current") flashUpToDate("Up to date ✓");
    },
    onError: (e) => setError(errorText(e, "The update didn't start")),
  });

  if (!detail.data) {
    if (!detail.isError) return <Loading label={`Loading ${name}…`} className="justify-center" />;
    if (detail.error instanceof ApiError && detail.error.status === 404) {
      return <p className="py-12 text-center text-ink-muted">No app named {name}. It may have been removed.</p>;
    }
    return <LoadError label={`Couldn't load ${name}.`} error={detail.error} onRetry={() => detail.refetch()} />;
  }

  const entry = catalog.data?.entries.find((e) => e.id === name);
  const status = statuses.data?.find((s) => s.name === name);
  const statusKnown = !!statuses.data;
  const isRunning = status?.state === "running";
  const deploying = (deployStatus.data?.current.running ?? false) || (status?.deploying ?? false);
  const neverDeployed = detail.data.app.currentRev === 0;

  // Same link logic as the advanced header: HTTPS hostname first, direct
  // port second.
  const routes = detail.data.spec.routes ?? [];
  const primaryHost = routes.length > 0 ? routeHost(name, routes[0].host, settings.data?.base_domain) : null;
  const openURL = primaryHost
    ? `https://${primaryHost}`
    : status?.hostPort
      ? `http://${window.location.hostname}:${status.hostPort}`
      : null;

  const btn =
    "rounded-md border border-edge px-4 py-2 text-sm font-medium text-ink hover:bg-fog-800 disabled:opacity-50";

  return (
    <div className="mx-auto max-w-xl pt-8">
      <div className="space-y-6 rounded-2xl border border-veil-edge bg-veil p-8 text-center">
        <div className="flex justify-center"><AppIcon entry={entry} size={72} /></div>
        <div className="space-y-2">
          <h1 className="flex items-center justify-center gap-3 text-2xl font-semibold text-ink">
            {entry?.title ?? name}
            <StateBadge status={status} known={statusKnown} error={statuses.isError} />
          </h1>
          {entry?.description && <p className="text-sm text-ink-muted">{entry.description}</p>}
        </div>

        {deploying ? (
          <p className="flex items-center justify-center gap-2 text-sm text-ink">
            <Spinner />
            Updating — this can take a few minutes…
          </p>
        ) : neverDeployed ? (
          <p className="text-sm text-ink-muted">
            This app isn't fully installed yet. Finish setting it up in advanced mode
            (avatar menu → Switch to advanced mode).
          </p>
        ) : !statusKnown ? (
          <p className="flex items-center justify-center gap-2 text-sm text-ink-muted">
            <Spinner tone="border-fog-500" />
            {statuses.isError ? "State unavailable" : "Checking state…"}
          </p>
        ) : (
          <div className="flex flex-wrap items-center justify-center gap-2">
            {isRunning ? (
              <>
                <button onClick={() => action.mutate("stop")} disabled={action.isPending} className={btn}>
                  {action.isPending && action.variables === "stop" ? "Stopping…" : "Stop"}
                </button>
                <button onClick={() => action.mutate("restart")} disabled={action.isPending} className={btn}>
                  {action.isPending && action.variables === "restart" ? "Restarting…" : "Restart"}
                </button>
              </>
            ) : (
              <button onClick={() => action.mutate("start")} disabled={action.isPending} className={btn}>
                {action.isPending && action.variables === "start" ? "Starting…" : "Start"}
              </button>
            )}
            <button onClick={() => update.mutate()} disabled={update.isPending} className={btn}>
              {update.isPending ? "Checking…" : upToDateNote || "Update"}
            </button>
            {isRunning && openURL && (
              <a href={openURL} target="_blank" rel="noreferrer"
                className="rounded-md bg-accent px-4 py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover">
                Open {entry?.title ?? name} ↗
              </a>
            )}
          </div>
        )}

        {error && <p className="text-sm text-warn">{error}</p>}
      </div>
    </div>
  );
}
