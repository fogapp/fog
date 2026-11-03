// Settings › Storage › Database. The shared Postgres cluster every
// database-backed app uses: its major, the live container, and the one
// button that moves it to the major this fog ships.

import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, type PostgresStatus } from "../../api";
import { StatusLine } from "../../components/Section";
import { Loading, Spinner, errorText } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import { formatBytes } from "../../lib/format";
import { paths } from "../../lib/paths";
import ContainerFacts from "./ContainerFacts";
import { SettingsPage } from "./Frame";

// The shared Postgres cluster's major, and the one button that moves it to
// the major this fog ships. The data directory decides what runs, so a fog
// release never changes the cluster by itself — this is the only door.
// During the move the apps that use the cluster are stopped, every row is
// counted before and after, and any mismatch rolls back automatically; the
// old data directory is kept (Orphaned data, below) until reclaimed.
export default function Database() {
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const [ask, dialog] = useConfirm();
  const status = useQuery({
    queryKey: ["postgres"],
    queryFn: () => get<PostgresStatus>("/api/system/postgres"),
    refetchInterval: (q) => (q.state.data?.upgrade.running ? 2000 : 30_000),
  });
  const st = status.data;
  const cluster = st?.cluster;
  const up = st?.upgrade;

  const upgrade = useMutation({
    mutationFn: () => post("/api/system/postgres/upgrade"),
    onSuccess: () => {
      setError("");
      return qc.invalidateQueries({ queryKey: ["postgres"] });
    },
    onError: (e) => setError(errorText(e, "Upgrade failed to start")),
  });

  const cancel = useMutation({
    mutationFn: () => post("/api/system/postgres/upgrade/cancel"),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["postgres"] }),
    onError: (e) => setError(errorText(e, "Could not cancel the upgrade")),
  });

  async function stop() {
    const ok = await ask({
      title: "Cancel the Postgres upgrade?",
      body: (
        <p>
          Before the old cluster is set aside, the apps simply come back on it. After that, the step in progress is
          stopped and fog rolls back to the old cluster, exactly as it does on any failure.
        </p>
      ),
      action: "Cancel the upgrade",
    });
    if (ok) cancel.mutate();
  }

  async function start() {
    if (!cluster) return;
    const ok = await ask({
      title: `Upgrade the shared Postgres from ${cluster.major} to ${cluster.targetMajor}?`,
      body: (
        <>
          <p>Every app that uses it stops for the few minutes this takes.</p>
          <p>
            Fog dumps every database, starts a fresh cluster, restores, and checks every table's row count before the
            apps come back — any mismatch rolls back to Postgres {cluster.major} automatically. The old data directory
            is kept until you reclaim it.
          </p>
        </>
      ),
      action: `Upgrade to Postgres ${cluster.targetMajor}`,
    });
    if (ok) upgrade.mutate();
  }

  // No verdict about the cluster until the status has actually arrived.
  const summary = !st
    ? status.isError ? "status unavailable" : "checking…"
    : cluster?.error
      ? cluster.error
      : cluster?.fresh
        ? `no cluster yet — the first database-backed app starts Postgres ${cluster.targetMajor}`
        : `Postgres ${cluster?.major} · ${formatBytes(cluster?.dataBytes ?? 0)} of data`;

  return (
    <SettingsPage
      description={
        <>
          The shared cluster every database-backed app uses. Its major is decided by the data on disk;
          this fog ships Postgres {cluster?.targetMajor ?? "…"}.
        </>
      }
      status={
        !st && !status.isError ? (
          <Loading label="Checking the cluster…" />
        ) : (
          // No status is no verdict: "status unavailable" must not sit
          // behind a green dot claiming the cluster is fine.
          <StatusLine
            tone={!st ? "idle" : cluster?.error || cluster?.upgradeAvailable || up?.error ? "warn" : cluster?.fresh ? "idle" : "ok"}
          >
            {summary}
            {cluster?.upgradeAvailable && !up?.running && ` · Postgres ${cluster.targetMajor} available`}
          </StatusLine>
        )
      }
    >
      <div className="max-w-2xl space-y-4">
        <ContainerFacts which="postgres" absent="No cluster container yet — it starts with the first app that needs a database." />
        {cluster?.upgradeAvailable && !up?.running && (
          <Button variant="primary" onClick={start} disabled={upgrade.isPending}>
            {upgrade.isPending ? "Starting…" : `Upgrade to Postgres ${cluster.targetMajor}`}
          </Button>
        )}
        {cluster && !cluster.fresh && !cluster.upgradeAvailable && !cluster.error && !up?.running && (
          <p className="text-sm text-accent">✓ On the current major.</p>
        )}
        {up?.running && (
          <div className="flex items-center gap-2 text-sm text-ink">
            <Spinner tone="border-fog-500" />
            <span>{up.stage || "Starting…"}</span>
            <Button onClick={stop} disabled={cancel.isPending}>
              {cancel.isPending ? "Cancelling…" : "Cancel"}
            </Button>
          </div>
        )}
        {/* The stage log stays after the run — the last line is the verdict,
            and on a failure the lines above it are the diagnosis. */}
        {up && (up.running || up.done) && up.lines.length > 0 && (
          <pre className="max-h-48 overflow-y-auto rounded-md bg-fog-950 p-2 text-xs text-ink-muted">
            {up.lines.slice(-30).join("\n")}
          </pre>
        )}
        {up?.done && !up.error && (
          <p className="text-sm text-accent">✓ Upgraded to Postgres {up.to}. The apps are back.</p>
        )}
        {up?.error && (
          <p className={`text-sm ${up.rolledBack ? "text-warn" : "text-warn"}`}>{up.error}</p>
        )}
        {cluster && cluster.retired.length > 0 && (
          <p className="text-xs text-ink-muted">
            Kept from earlier upgrades: {cluster.retired.map((r) => `${r.path.split("/").pop()} (${formatBytes(r.sizeBytes)})`).join(", ")}.
            Reclaim under <Link to={paths.settings("storage/orphans")} className="underline">Orphaned data</Link> once you are satisfied with the migrated data.
          </p>
        )}
        {error && <p className="text-sm text-warn">{error}</p>}
        {dialog}
      </div>
    </SettingsPage>
  );
}

