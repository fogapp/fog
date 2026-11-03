// The review an imported app opens on. A manifest is somebody else's spec,
// and a spec can bind the Docker socket: nothing deploys until the operator
// has read what this one reaches — every host mount, published port, grant
// and acknowledgement — and said yes to the revision they read.
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { get, post, type AppImportResult, type AppReview } from "../../api";
import { LoadError, Loading } from "../../components/Async";
import { useAction } from "../../lib/useAction";

function reviewKey(name: string) {
  return ["app-review", name] as const;
}

export function ImportReview({ name, isGit, onConfirmed, onRemove }: {
  name: string; isGit: boolean; onConfirmed: () => void; onRemove: () => void;
}) {
  const review = useQuery({
    queryKey: reviewKey(name),
    queryFn: () => get<AppImportResult>(`/api/apps/${name}/review`),
  });
  const { busy, error, run } = useAction();
  // The operator's yes to taking over what an earlier app of this name
  // left behind; confirming waits for it when there is any.
  const [adopt, setAdopt] = useState(false);

  if (!review.data) {
    return review.isError
      ? <LoadError label="Couldn't load this app's review." error={review.error} onRetry={() => review.refetch()} />
      : <Loading label="Loading the review…" className="justify-center" />;
  }
  const { rev, review: r } = review.data;

  async function confirm() {
    const ok = await run(
      () => post(`/api/apps/${name}/review/confirm`, { rev, deploy: !isGit, adoptLeftovers: adopt }),
      "Couldn't confirm the review",
    );
    if (ok !== undefined) onConfirmed();
    else review.refetch();
  }

  return (
    <div className="mx-auto max-w-2xl space-y-5">
      <div className="space-y-1">
        <p className="font-medium text-ink">Review {name} before it runs</p>
        <p className="text-sm text-ink-muted">
          This app was imported from a manifest and hasn't been deployed. Read what it reaches beyond its own
          data — nothing runs until you confirm.
        </p>
      </div>

      {r.warnings.length > 0 && (
        <div className="space-y-1 rounded-lg border border-warn/30 bg-warn/10 p-4 text-sm text-warn">
          <p className="font-medium">This app can take over the server</p>
          <ul className="list-disc space-y-1 pl-5">
            {r.warnings.map((w) => <li key={w}>{w}</li>)}
          </ul>
        </div>
      )}

      {r.leftovers.length > 0 && (
        <div role="alert" className="space-y-2 rounded-lg border border-warn/30 bg-warn/10 p-4 text-sm text-warn">
          <p className="font-medium">{name} would take over an earlier app's data</p>
          <p>
            An app named {name} was deleted with its data kept. This one would get all of it — files, settings,
            accounts and any credentials in them:
          </p>
          <ul className="list-disc space-y-1 pl-5">
            {r.leftovers.map((l) => <li key={l}>{l}</li>)}
          </ul>
          <label className="flex items-start gap-2 text-ink">
            <input type="checkbox" className="mt-1" checked={adopt} onChange={(ev) => setAdopt(ev.target.checked)} />
            <span>Let this import take over that data. If it isn't the same app, remove it instead and delete the old data first.</span>
          </label>
        </div>
      )}

      <ReviewTable review={r} />

      {error && <p className="text-sm text-warn">{error}</p>}
      <div className="flex flex-wrap items-center gap-3">
        <button type="button" onClick={confirm} disabled={busy || (r.leftovers.length > 0 && !adopt)}
          className="rounded-md bg-accent px-4 py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
          {busy ? "Confirming…" : isGit ? `I've reviewed it — continue to setup` : `I've reviewed it — deploy ${name}`}
        </button>
        <button type="button" onClick={onRemove} className="text-sm text-ink-muted hover:text-warn">
          Don't install — remove it
        </button>
      </div>
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[9rem_minmax(0,1fr)] gap-3 border-t border-hair py-2 text-sm first:border-t-0">
      <span className="text-ink-muted">{label}</span>
      <div className="min-w-0 break-words text-ink">{children}</div>
    </div>
  );
}

const none = <span className="text-ink-muted">none</span>;

function ReviewTable({ review: r }: { review: AppReview }) {
  return (
    <div className="rounded-lg border border-hair px-4 py-2">
      <Row label="Runs">{r.source || none}</Row>
      <Row label="Host folders">
        {r.mounts.length === 0 ? none : (
          <ul className="space-y-1">
            {r.mounts.map((m) => (
              <li key={(m.service || "") + "/" + m.volume} className={m.dangerous || m.otherApp || m.follows ? "text-warn" : ""}>
                <code>{m.host ?? `Drive: ${m.files}`}</code> → <code>{m.mount}</code>
                {m.readOnly ? " (read-only)" : " (read-write)"}
                {m.service && <> in sidecar <code>{m.service}</code></>}
                {m.owner && <> — fog will chown it to <code>{m.owner}</code></>}
                {m.follows && <> — follows <code>{m.follows}</code></>}
                {m.otherApp && !m.dangerous && <div className="text-xs">{m.otherApp}'s own data folder — its files, credentials included</div>}
                {m.dangerous && <div className="text-xs">{m.dangerous}{m.acknowledged ? " — acknowledged by the manifest" : ""}</div>}
              </li>
            ))}
          </ul>
        )}
      </Row>
      <Row label="Host ports">
        {r.ports.length === 0 ? none : r.ports.map((p) => (
          <div key={`${p.hostPort}/${p.protocol}`} className={p.lowPort ? "text-warn" : ""}>
            {p.hostPort}/{p.protocol} → {p.port}{p.lowPort ? " (a privileged port on the host)" : ""}
          </div>
        ))}
      </Row>
      <Row label="Web addresses">
        {r.routes.length === 0 ? none : r.routes.map((rt, i) => (
          <div key={i}>
            {rt.host || "default address"} → port {rt.port}
            {rt.auth ? " · household sign-in" : " · public"}{rt.lanOnly ? " · home network only" : ""}
          </div>
        ))}
      </Row>
      <Row label="Network">
        {r.network === "sealed" ? "sealed — no way out of this machine" : "its own network, with internet access"}
        {r.sealedPeer ? "; sealed apps may reach it" : ""}
      </Row>
      <Row label="Can reach">
        {r.reaches.length ? r.reaches.join(", ") : <span className="text-ink-muted">no other app — only fog</span>}
        {r.reachAsked.length > 0 && (
          <div className="text-xs text-warn">
            Asks to call {r.reachAsked.join(", ")} on the network — confirming approves it, including an app by that
            name installed later.
          </div>
        )}
      </Row>
      {r.rawSockets && (
        <Row label="Raw sockets">
          <span className="text-warn">kept — it can forge addresses on its network</span>
        </Row>
      )}
      <Row label="Its settings point at">
        {r.envLinks.length
          ? <ul className="space-y-0.5 font-mono text-xs">{r.envLinks.map((l) => <li key={l} className="break-all">{l}</li>)}</ul>
          : none}
      </Row>
      <Row label="Reads from fog">{r.uses.length ? r.uses.join(", ") : none}</Row>
      <Row label="Connects to">{r.wants.length ? r.wants.join(", ") : none}</Row>
      <Row label="Shares folders">{r.shares.length ? r.shares.join(", ") : none}</Row>
      <Row label="Secrets it asks for">{r.secrets.length ? r.secrets.join(", ") : none}</Row>
      <Row label="Variables it sets">{r.env.length ? r.env.join(", ") : none}</Row>
      <Row label="Also">
        {[r.postgres && "a database in fog's Postgres", r.signin && "household sign-in (an OpenID client)"]
          .filter(Boolean).join("; ") || none}
      </Row>
      {r.release.length > 0 && <Row label="Runs on every deploy"><code>{r.release.join(" ")}</code></Row>}
      {r.execFiles.length > 0 && <Row label="Executable files">{r.execFiles.map((f) => <div key={f}><code>{f}</code></div>)}</Row>}
      {r.acknowledge.length > 0 && (
        <Row label="Acknowledged">
          <span className="text-warn">{r.acknowledge.join(", ")}</span>
        </Row>
      )}
    </div>
  );
}
