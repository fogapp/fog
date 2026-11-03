// One inbound subscription, in full: what this box holds of it, how the
// sync is doing, and every action on it — sync now, pause, acknowledge
// changed terms, switch detail, back it up, remove or adopt the copy.

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { del, post, put, type FedPeer, type FileFedSubscription } from "../../api";
import { errorText } from "../../components/Async";
import Button from "../../components/Button";
import Modal from "../../components/Modal";
import Toggle from "../../components/Toggle";
import { formatBytes, peerLabel, plural, timeAgo } from "../../lib/format";
import { paths } from "../../lib/paths";
import { filesKey } from "../../lib/useFiles";
import { Dot } from "../app/cockpit";
import { subState } from "./data";
import { federationQuery, subscriptionsQuery } from "../../queries";

export function SubscriptionModal({ sub, peer, onClose }: { sub: FileFedSubscription; peer?: FedPeer; onClose: () => void }) {
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const [confirm, setConfirm] = useState<"remove" | "adopt" | null>(null);
  const [mounted, setMounted] = useState<{ mode: "remove" | "adopt"; text: string } | null>(null);
  const name = peer ? peerLabel(peer) : sub.peer ?? "the peer";
  const refresh = () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: subscriptionsQuery.queryKey }),
      qc.invalidateQueries({ queryKey: federationQuery.queryKey }),
      qc.invalidateQueries({ queryKey: filesKey }),
    ]);
  const action = useMutation({
    mutationFn: (verb: string) => post(`/api/files/subscriptions/${sub.id}/${verb}`),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e) => setError(errorText(e, "That did not work")),
  });
  const update = useMutation({
    mutationFn: (patch: { backup?: boolean; detail?: "full" | "previews" }) => put(`/api/files/subscriptions/${sub.id}`, patch),
    onSuccess: () => refresh(),
    onError: (e) => setError(errorText(e, "That did not work")),
  });
  const drop = useMutation({
    mutationFn: ({ mode, force }: { mode: "remove" | "adopt"; force?: boolean }) =>
      del(`/api/files/subscriptions/${sub.id}?mode=${mode}${force ? "&force=1" : ""}`),
    onSuccess: async () => { await refresh(); onClose(); },
    onError: (e, vars) => {
      const text = errorText(e, "That did not work");
      if (text.includes("mounts this copy")) setMounted({ mode: vars.mode, text });
      else setError(text);
    },
  });
  const st = subState(sub);

  return (
    <Modal title={sub.title} onClose={onClose}>
      <div className="space-y-4 text-sm">
        <p className="inline-flex flex-wrap items-center gap-2 text-hint text-ink-muted">
          from <span className="text-ink">{name}</span> · in{" "}
          <Link to={paths.files(sub.folder)} className="font-mono text-ink hover:text-ink">Drive / {sub.folder}</Link>
          {sub.mountedBy && sub.mountedBy.length > 0 && <> · mounted by {sub.mountedBy.join(", ")}</>}
        </p>
        <p className="text-ink">{sub.acceptedImpact}</p>
        <p className="text-hint text-ink-muted">
          {sub.detailOffered === "listing"
            ? `A listing: names, sizes and dates only — the files stay with ${name}.`
            : sub.detailWanted === "full"
              ? `Full copy: ${plural(sub.hydratedFiles, "file")} on this box`
              : `Previews: ${sub.hydratedFiles.toLocaleString()} of ${plural(sub.files, "file")} on this box (${formatBytes(sub.hydratedBytes)}) — the rest fetch when opened`}
          {sub.bytesFetched > 0 ? ` · ${formatBytes(sub.bytesFetched)} fetched${sub.lastFetchedAt ? `, last ${timeAgo(sub.lastFetchedAt)}` : ""}` : " · nothing fetched yet"}
          {sub.detailWanted === "full" && sub.files > sub.hydratedFiles && sub.status !== "revoked" && <> · {plural(sub.files - sub.hydratedFiles, "file")} behind</>}
          {" · "}rev {sub.revisionSeen}
        </p>
        <p className={`inline-flex items-center gap-2 text-hint ${st.tone === "warn" ? "text-warn" : st.tone === "ok" ? "text-accent" : "text-ink-muted"}`}>
          <Dot tone={st.tone === "warn" ? "warn" : st.tone === "ok" ? "ok" : "off"} />
          {st.text}
          {sub.status === "error" && sub.lastError && <span className="text-ink-muted"> — {sub.lastError}</span>}
          {sub.status === "revoked" && <span className="text-ink-muted"> — your copy stays until you decide</span>}
        </p>
        {sub.status === "offer_changed" && (
          <p className="rounded-md bg-warn/10 px-3 py-2 text-hint text-warn">{name} widened this share. Now: {sub.currentImpact}</p>
        )}
        {mounted && (
          <p className="flex flex-wrap items-center gap-2 rounded-md bg-warn/10 px-3 py-2 text-hint text-warn">
            {mounted.text}
            <Button size="sm" variant="danger" onClick={() => drop.mutate({ mode: mounted.mode, force: true })} disabled={drop.isPending}>Remove anyway</Button>
            <Button size="sm" variant="ghost" onClick={() => setMounted(null)}>Keep</Button>
          </p>
        )}

        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-hair pt-4">
          {sub.detailOffered !== "listing" && sub.status !== "revoked" && (
            <Toggle
              checked={sub.detailWanted === "full"}
              disabled={update.isPending}
              onChange={(on) => update.mutate({ detail: on ? "full" : "previews" })}
              label="Keep a full copy"
            />
          )}
          <Toggle checked={sub.backup} disabled={update.isPending} onChange={(on) => update.mutate({ backup: on })} label="Back up my copy" />
        </div>

        <div className="flex flex-wrap items-center gap-2">
          {sub.status === "offer_changed" && (
            <Button size="sm" variant="primary" onClick={() => action.mutate("acknowledge")} disabled={action.isPending}>Acknowledge the new terms</Button>
          )}
          {(sub.status === "active" || sub.status === "error") && (
            <>
              <Button size="sm" onClick={() => action.mutate("sync")} disabled={action.isPending}>Sync now</Button>
              <Button size="sm" variant="ghost" onClick={() => action.mutate("pause")} disabled={action.isPending}>Pause</Button>
            </>
          )}
          {sub.status === "paused" && (
            <Button size="sm" onClick={() => action.mutate("resume")} disabled={action.isPending}>Resume</Button>
          )}
          <span className="ml-auto inline-flex flex-wrap items-center gap-2">
            {confirm ? (
              <>
                <span className="text-hint text-warn">
                  {confirm === "remove" ? "Remove the copy from this box?" : "Keep the files as your own? They become ordinary files here."}
                </span>
                <Button size="sm" variant={confirm === "remove" ? "danger" : "primary"} onClick={() => drop.mutate({ mode: confirm })} disabled={drop.isPending}>Confirm</Button>
                <Button size="sm" variant="ghost" onClick={() => setConfirm(null)}>Cancel</Button>
              </>
            ) : (
              <>
                {sub.status === "revoked" && (
                  <Button size="sm" variant="ghost" onClick={() => setConfirm("adopt")}>Keep as my own</Button>
                )}
                <Button size="sm" variant="danger" onClick={() => setConfirm("remove")}>Remove my copy</Button>
              </>
            )}
          </span>
        </div>
        {error && <p className="text-hint text-warn">{error}</p>}
      </div>
    </Modal>
  );
}
