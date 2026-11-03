// The page's dialogs: minting a code, entering one, removing a peer. Each
// is one decision with its consequence stated before the button. This
// fog's own identity is the Config tab, not a dialog.

import { useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { del, post, type FedInvitePreview, type FedPeer } from "../../api";
import { errorText } from "../../components/Async";
import Button from "../../components/Button";
import { copyText } from "../../components/CopyButton";
import Field from "../../components/Field";
import Modal from "../../components/Modal";
import Toggle from "../../components/Toggle";
import { peerLabel, plural } from "../../lib/format";
import { Dot } from "../app/cockpit";
import { federationQuery } from "../../queries";

// InviteModal mints one code and shows it once — fogd keeps only a digest
// of the secret inside it, so there is no second look.
export function InviteModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const [shareErrors, setShareErrors] = useState(true);
  const [minted, setMinted] = useState<string | null>(null);
  const [copied, setCopied] = useState<"idle" | "ok" | "fail">("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const invite = useMutation({
    mutationFn: () =>
      post<{ code: string }>("/api/federation/invites", { grants: shareErrors ? [{ capability: "flare.ingest" }] : [] }),
    onSuccess: (res) => {
      setError("");
      setMinted(res.code);
      return qc.invalidateQueries({ queryKey: federationQuery.queryKey });
    },
    onError: (e) => setError(errorText(e, "Couldn't create the invite")),
  });

  const copy = async () => {
    if (!minted) return;
    const ok = await copyText(minted);
    setCopied(ok ? "ok" : "fail");
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied("idle"), 1500);
  };

  return (
    <Modal title="Invite a peer" onClose={onClose} width="max-w-md">
      <p className="text-sm text-ink">They enter this code on their own fog. It works once, within 24 hours.</p>
      {minted ? (
        <div className="mt-4 rounded border border-veil-edge bg-code p-[18px] text-center font-mono text-[22px] tracking-[.12em] text-accent">
          {minted}
        </div>
      ) : (
        <div className="mt-4">
          <Toggle checked={shareErrors} onChange={setShareErrors} label="Let this peer send its errors here" />
        </div>
      )}
      {minted && <p className="mt-3 text-hint text-ink-muted">It won't be shown again — copy it now.</p>}
      {error && <p className="mt-3 text-hint text-warn">{error}</p>}
      <div className="mt-[18px] flex justify-end gap-2">
        {minted ? (
          <>
            <Button variant="ghost" onClick={onClose}>Done</Button>
            <Button variant="primary" onClick={copy}>{copied === "ok" ? "Copied ✓" : copied === "fail" ? "Copy failed" : "Copy code"}</Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose}>Cancel</Button>
            <Button variant="primary" onClick={() => invite.mutate()} disabled={invite.isPending}>
              {invite.isPending ? "Creating…" : "Create code"}
            </Button>
          </>
        )}
      </div>
    </Modal>
  );
}

// JoinModal: their code, pasted here. The code is decoded locally first
// (no network to the peer, no secret spent) so the joiner sees who they
// are about to pair with; pairing completes on submit.
export function JoinModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [preview, setPreview] = useState<FedInvitePreview | null>(null);
  useEffect(() => {
    const c = code.trim();
    if (!c.startsWith("FOG-")) {
      setPreview(null);
      return;
    }
    let cancelled = false;
    const t = setTimeout(() => {
      post<FedInvitePreview>("/api/federation/join/preview", { code: c })
        .then((p) => !cancelled && setPreview(p))
        .catch(() => !cancelled && setPreview(null));
    }, 250);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [code]);
  const join = useMutation({
    mutationFn: (c: string) => post<FedPeer>("/api/federation/join", { code: c }),
    onSuccess: async () => {
      setError("");
      await qc.invalidateQueries({ queryKey: federationQuery.queryKey });
      onClose();
    },
    onError: (e) => setError(errorText(e, "Couldn't join — check the code")),
  });

  return (
    <Modal title="Join with a code" onClose={onClose} width="max-w-sm">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (code.trim()) join.mutate(code.trim());
        }}
      >
        <Field
          label="Invite code"
          autoFocus
          value={code}
          onChange={(e) => setCode(e.target.value)}
          placeholder="FOG-0000-0000"
          aria-invalid={!!error}
          hint={
            error
              ? error
              : preview
                ? <>Pair with <span className="text-ink">{preview.name || "this household"}</span>{preview.url && <span> · {preview.url}</span>}</>
                : "Paste the code the other fog's operator gave you."
          }
        />
        <div className="mt-[18px] flex justify-end gap-2">
          <Button variant="ghost" type="button" onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" disabled={join.isPending || !code.trim()}>
            {join.isPending ? "Pairing…" : preview?.name ? `Join ${preview.name}` : "Join"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

// RemoveModal: trust drops both ways, and the consequence is counted from
// what actually exists — the folders shared and held, and the error
// groups they relayed — never from a role fog does not have.
export function RemoveModal({ peer, holds, shares, onClose, onRemoved }: {
  peer: FedPeer;
  holds: number;
  shares: number;
  onClose: () => void;
  onRemoved: () => void;
}) {
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const remove = useMutation({
    mutationFn: () => del(`/api/federation/peers/${peer.id}`),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: federationQuery.queryKey });
      onRemoved();
    },
    onError: (e) => setError(errorText(e, "Couldn't remove the peer")),
  });
  const name = peerLabel(peer);
  const ends: string[] = [];
  if (holds > 0) ends.push(`${plural(holds, "folder")} you share`);
  if (shares > 0) ends.push(`${plural(shares, "subscription")} from them`);
  return (
    <Modal title={`Remove ${name}`} onClose={onClose} width="max-w-md">
      <p className="text-sm text-ink">
        The trust is dropped both ways. Folders they hold for you stay on their machine until they delete them;
        copies of theirs stay here until you remove them.
      </p>
      <p className="mt-3.5 inline-flex items-center gap-2 text-hint text-warn">
        <Dot tone="warn" />
        {ends.length > 0 ? `${ends.join(" and ")} end, and ` : "Their access ends immediately, and "}every error group they sent is deleted.
      </p>
      {error && <p className="mt-3 text-hint text-warn">{error}</p>}
      <div className="mt-[18px] flex justify-end gap-2">
        <Button variant="ghost" onClick={onClose}>Cancel</Button>
        <Button variant="danger" onClick={() => remove.mutate()} disabled={remove.isPending}>
          {remove.isPending ? "Removing…" : "Remove peer"}
        </Button>
      </div>
    </Modal>
  );
}

