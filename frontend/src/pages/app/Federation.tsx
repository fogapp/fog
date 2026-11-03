// The federation tab, grouped by the operator's questions: who holds a
// copy, what happens if this machine dies, who can reach the app, and
// which of its folders are shared out. Fog has no per-app failover and
// says so — the "if this machine goes down" answer is the copies that
// actually exist elsewhere, never a safety net it does not have.

import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, type AppDetail as AppDetailT, type AppStatus, type FileFedShare } from "../../api";
import { LoadError, Loading, errorText } from "../../components/Async";
import Button, { buttonClass } from "../../components/Button";
import CopyButton from "../../components/CopyButton";
import { formatBytes, peerLabel, timeAgo } from "../../lib/format";
import { filesKey } from "../../lib/useFiles";
import { paths } from "../../lib/paths";
import { useBackupsStatus, type AppVolume } from "../../lib/useAppData";
import { useCopies } from "./Standing";
import { Dot, Group, Head, Kv, Pill, Row, Table, Veil, whenShort } from "./cockpit";
import { federationQuery, peerSharesQuery } from "../../queries";

export function FederationTab({ name, detail, status, volumes, openURL, onShare }: {
  name: string;
  detail: AppDetailT;
  status?: AppStatus;
  volumes: AppVolume[];
  openURL: string | null;
  onShare: (path?: string) => void;
}) {
  const { copies, known, fed, offsite } = useCopies(name, volumes);
  const backups = useBackupsStatus();
  const mine = backups.data?.apps.find((a) => a.app === name);
  const selfName = fed.data?.identity.name ?? "this fog";
  const driveFolders = volumes.filter((v) => v.rel !== undefined);
  const totalBytes = driveFolders.reduce((n, v) => n + (v.stat?.size ?? 0), 0);
  const sized = driveFolders.length > 0 && driveFolders.every((v) => v.statPhase === "ready");

  return (
    <div>
      <Group title="Who holds a copy" first right={driveFolders.length > 0 ? <Button size="sm" onClick={() => onShare(driveFolders[0].rel)}>Share a folder…</Button> : undefined}>
        <Veil className="px-5 py-[18px]">
          <div className="grid gap-0 md:grid-cols-[170px_minmax(0,1fr)]">
            <div className="flex flex-col justify-center gap-2 border-b border-hair pb-4 md:border-b-0 md:border-r md:pb-0 md:pr-5">
              <span className="inline-flex items-center gap-2 text-cell text-ink">
                <Dot tone={status?.state === "running" ? "ok" : "off"} />{selfName}
              </span>
              <span className="text-hint text-ink-muted">This fog{sized ? ` · ${formatBytes(totalBytes)} on Drive` : ""}</span>
              <span className="text-hint text-ink-muted">{status?.state === "running" ? "Serving the app" : "The app is not running"}</span>
            </div>
            <div className="pt-4 md:pl-5 md:pt-0">
              {copies.length === 0 ? (
                <p className="text-hint text-ink-muted">
                  {known ? "No other machine holds a copy of this app's data." : "Looking for copies…"}
                  {known && driveFolders.length > 0 && <> Share one of its Drive folders with a peer, or set up an offsite locker.</>}
                </p>
              ) : (
                copies.map((c) => (
                  <div key={c.key} className="grid items-center gap-3.5 rounded-md px-2 py-2.5 hover:bg-hover" style={{ gridTemplateColumns: "minmax(0,1fr) minmax(0,1.3fr) auto" }}>
                    <span className="inline-flex min-w-0 items-center gap-2 text-cell text-ink">
                      <span className="font-mono text-ink-faint">└─</span>
                      <Dot tone={c.online === false ? "off" : "ok"} />
                      <span className="truncate">{c.label}</span>
                    </span>
                    <span className="truncate text-hint text-ink-muted">{copyDetail(c, offsite.data?.rows ?? [], fed.data?.peers ?? [])}</span>
                    <span className="text-right"><Pill tone={c.online === false ? "off" : "neutral"}>{c.role.split(" ")[0] === "Holds" ? "Copy" : c.role.split(" ")[0]}</Pill></span>
                  </div>
                ))
              )}
            </div>
          </div>
        </Veil>
      </Group>

      <Group title="If this machine goes down">
        <Veil className="px-[18px] py-4">
          {!mine && !backups.isError ? (
            <Loading label="Reading where this app's snapshots are…" />
          ) : (
            <>
              <div className="text-cell text-ink">
                {mine?.offsite === "verified" || mine?.offsite === "stored"
                  ? <>The newest snapshot is in the locker{mine.offsite === "verified" ? " and verified" : ""}.</>
                  : mine?.offsite === "uploading" || mine?.offsite === "pending"
                    ? <>The newest snapshot is on its way to the locker.</>
                    : mine?.enabled
                      ? <>Snapshots stay on this machine only.</>
                      : <>This app is not backed up.</>}
              </div>
              <div className="mt-[5px] text-hint text-ink-muted">
                {mine?.offsite === "verified" || mine?.offsite === "stored"
                  ? "Recover it from the locker on a fresh install with the recovery kit; the app's spec and its data come back together."
                  : mine?.enabled
                    ? "An offsite locker would carry the snapshots off this box; a peer share would carry a Drive folder."
                    : "Turn on backups on the Drive tab to have something to recover from."}
              </div>
              <div className="mt-3 flex flex-wrap gap-2">
                <Link to={paths.settings("storage/offsite")} className={buttonClass("secondary", "sm")}>
                  Offsite lockers
                </Link>
                <Link to={paths.app(name, "drive", { tab: "schedule" })} className="inline-flex items-center rounded-md px-2.5 py-1 text-xs font-medium text-ink-muted hover:bg-hover hover:text-ink">
                  Backup schedule
                </Link>
              </div>
              {mine?.stale && (
                <div className="mt-3 inline-flex items-center gap-2 text-hint text-warn">
                  <Dot tone="warn" />
                  {mine.lastAt ? `The last snapshot is ${timeAgo(mine.lastAt)} old — recovery would lose what came after.` : "Scheduled, but no snapshot has been taken — there is nothing to recover yet."}
                </div>
              )}
              <div className="mt-3 text-hint text-ink-muted">
                Fog does not fail an app over to a peer on its own; a copy elsewhere is a recovery point, not a standby.
              </div>
            </>
          )}
        </Veil>
      </Group>

      <Group title="Reaching the app">
        <Veil className="px-[18px] py-4">
          {openURL ? (
            <Kv label="On this network" action={<CopyButton text={openURL} />}>{openURL}</Kv>
          ) : (
            <Kv label="On this network"><span className="font-sans text-ink-muted">no web address{status?.hostPort ? "" : " — add a route on Config"}</span></Kv>
          )}
          {(status?.hosts ?? []).filter((h) => `https://${h}` !== openURL).map((h) => (
            <div key={h} className="mt-2.5"><Kv label="" action={<CopyButton text={`https://${h}`} />}>https://{h}</Kv></div>
          ))}
          {status?.hostPort && openURL !== `http://${window.location.hostname}:${status.hostPort}` ? (
            <div className="mt-2.5"><Kv label="Direct port" action={<CopyButton text={`http://${window.location.hostname}:${status.hostPort}`} />}>{window.location.hostname}:{status.hostPort}</Kv></div>
          ) : null}
          <div className="my-3.5 h-px bg-divider" />
          <Reachability />
          {detail.federation?.contracts?.length ? (
            <div className="mt-3 text-hint text-ink-muted">
              Implements <span className="font-mono text-ink">{detail.federation.contracts.join(", ")}</span> across Connect.
              {detail.federation.hostChange && <span className="text-warn"> If its host changes: {detail.federation.hostChange}</span>}
            </div>
          ) : null}
        </Veil>
      </Group>

      <FolderShares name={name} volumes={volumes} onShare={onShare} />
    </div>
  );
}

function copyDetail(c: { key: string; role: string }, rows: { target: string; createdAt: string; verifiedAt?: string }[], peers: { name: string; label?: string; online: boolean; lastSeenAt?: string }[]): string {
  if (c.key.startsWith("locker:")) {
    const mine = rows.filter((r) => r.target === c.key.slice(7));
    const newest = mine.reduce<typeof mine[number] | undefined>((a, r) => (!a || r.createdAt > a.createdAt ? r : a), undefined);
    return newest ? `${mine.length} snapshot${mine.length === 1 ? "" : "s"} · newest ${whenShort(newest.createdAt)}${newest.verifiedAt ? " · verified" : ""}` : "";
  }
  const label = c.key.slice(c.key.indexOf(":") + 1);
  const peer = peers.find((p) => p.name === label || peerLabel(p) === label);
  const seen = peer?.lastSeenAt ? `seen ${timeAgo(peer.lastSeenAt)}` : "";
  return peer ? `${c.role} · ${peer.online ? "online" : seen || "unreachable"}` : c.role;
}

// Whether other households can dial this box at all — the instance-level
// fact from the Connect page, said here because it is the answer to
// "can my peer reach this app".
function Reachability() {
  const fed = useQuery({ ...federationQuery, refetchInterval: 10_000 });
  if (!fed.data) {
    return fed.isError
      ? <p className="text-hint text-warn">Couldn't read Connect reachability.</p>
      : <Loading label="Checking Connect reachability…" />;
  }
  const r = fed.data.reachability;
  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <div className="text-cell text-ink">
          Through Connect {r.reachable ? <span className="text-accent">reachable</span> : <span className="text-ink-muted">not reachable</span>}
        </div>
        <div className="mt-[5px] text-hint text-ink-muted">
          {r.reachable
            ? <>Peers dial <span className="font-mono">{r.url}</span> ({r.via === "domain" ? "your domain" : "the advertised URL"}){r.tunnel.running ? " · through the tunnel, no router ports opened" : ""}.</>
            : r.advice || "No URL is advertised and no tunnel is running."}
        </div>
      </div>
      <Link to={paths.federation} className="shrink-0 text-xs text-ink-muted hover:text-ink">Connect ›</Link>
    </div>
  );
}

function FolderShares({ name, volumes, onShare }: { name: string; volumes: AppVolume[]; onShare: (path?: string) => void }) {
  const qc = useQueryClient();
  const shares = useQuery(peerSharesQuery);
  const [error, setError] = useState("");
  const folders = volumes.flatMap((v) => (v.rel !== undefined ? [v.rel] : []));
  const under = (p: string) => folders.some((f) => p === f || p.startsWith(f + "/") || f.startsWith(p + "/"));
  const mine = (shares.data?.shares ?? []).filter((s) => under(s.path));
  const revoke = useMutation({
    mutationFn: (id: number) => del(`/api/files/peer-shares/${id}`),
    onSuccess: async () => {
      setError("");
      await Promise.all([
        qc.invalidateQueries({ queryKey: peerSharesQuery.queryKey }),
        qc.invalidateQueries({ queryKey: filesKey }),
        qc.invalidateQueries({ queryKey: federationQuery.queryKey }),
      ]);
    },
    onError: (e) => setError(errorText(e, "Couldn't revoke the share")),
  });
  const cols = "1.3fr 1fr 110px 130px 80px";
  return (
    <Group
      title="Folder shares"
      right={
        <>
          {shares.data && <span className="text-hint text-ink-muted">{mine.length} active</span>}
          {folders.length > 0 && <Button size="sm" onClick={() => onShare(folders[0])}>Share a folder…</Button>}
        </>
      }
    >
      {error && <p className="mb-2 text-hint text-warn">{error}</p>}
      {!shares.data ? (
        shares.isError
          ? <LoadError label="Couldn't load this app's folder shares." error={shares.error} onRetry={() => shares.refetch()} />
          : <Loading label="Loading folder shares…" />
      ) : folders.length === 0 ? (
        <p className="text-hint text-ink-muted">This app has no Drive folders, so there is nothing of it to share. <Link to={paths.app(name, "config")} className="text-accent hover:text-accent-hover">Move a volume into Drive</Link> to change that.</p>
      ) : mine.length === 0 ? (
        <p className="text-hint text-ink-muted">None of this app's folders is shared with a peer.</p>
      ) : (
        <Table>
          <Head cols={cols}><span>Folder</span><span>Peer</span><span>Mode</span><span>Since</span><span /></Head>
          {mine.map((s: FileFedShare) => (
            <Row key={s.id} cols={cols}>
              <span className="min-w-0 truncate font-mono text-ink" title={s.path}>{s.path.split("/").join(" / ")}</span>
              <span className="truncate text-ink-muted">{s.peer}</span>
              <span className="text-ink-muted">{s.mode === "contribute" ? "Read and write" : "Read only"}{s.detail !== "full" ? ` · ${s.detail}` : ""}</span>
              <span className="text-ink-muted">{whenShort(s.createdAt)}</span>
              <span className="text-right">
                <button type="button" onClick={() => revoke.mutate(s.id)} disabled={revoke.isPending}
                  className="text-xs text-ink-faint hover:text-warn disabled:opacity-50">
                  {revoke.isPending && revoke.variables === s.id ? "Revoking…" : "Revoke"}
                </button>
              </span>
            </Row>
          ))}
        </Table>
      )}
    </Group>
  );
}
