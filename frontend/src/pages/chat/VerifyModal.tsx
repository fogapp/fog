// Verifying a person (docs/chat-contract.md): two people compare the
// safety code of an identity key — in the kitchen, over the phone — and mark
// each other verified. The mark lives on this device, never on fog.

import { useEffect, useState } from "react";
import type { ChatClient } from "@fog/chat-sdk";
import Modal from "../../components/Modal";
import CopyButton from "../../components/CopyButton";
import { useAction } from "../../lib/useAction";

// Both codes come from what this device holds (ChatClient.safetyCodes):
// its own identity seed, and the key it pinned for them — never the keys fog
// serves now, which a fog swapping keys would make agree with themselves.
export default function VerifyModal({
  client, addr, name, onClose,
}: {
  client: ChatClient;
  addr: string;
  name: string;
  onClose: () => void;
}) {
  const { run, busy, error } = useAction();
  const [codes, setCodes] = useState<Awaited<ReturnType<ChatClient["safetyCodes"]>> | null>(null);
  useEffect(() => {
    let alive = true;
    client.safetyCodes(addr).then((c) => alive && setCodes(c)).catch(() => {});
    return () => {
      alive = false;
    };
  }, [client, addr]);
  const theirs = codes?.theirs ?? "";
  const mine = codes?.mine ?? "";

  async function confirm() {
    await run(async () => {
      if (!codes?.theirKey) throw new Error("no identity key to verify");
      await client.markVerified(addr, codes.theirKey);
      onClose();
    }, "Couldn't save the mark", { holdOnSuccess: true });
  }

  return (
    <Modal title={`Verify ${name}`} onClose={onClose}>
      <div className="space-y-4 text-sm">
        <p className="text-hint leading-relaxed text-ink-muted">
          Ask {name} to open your entry under About this room on their device and read out their safety code. If it matches the one
          below, nobody is sitting between you — not fog, not either household's operator.
        </p>
        {(codes?.ownSwapped || codes?.theirsSwapped) && (
          <p role="alert" className="rounded-md border border-warn/40 bg-warn/10 px-2 py-1.5 text-hint text-warn">
            ⚠ Fog serves a different identity key {codes.ownSwapped ? "for you" : `for ${name}`} than the one this device holds. The codes
            below are from the keys this device holds; if they don't match what {name} reads out, someone is in between.
          </p>
        )}
        {codes && !codes.theirsPinned && theirs && (
          <p className="text-hint text-ink-muted">This device has no key of {name}'s yet: the code below is the one fog serves. A match in person is what makes it theirs.</p>
        )}
        <div className="space-y-1">
          <span className="field-label">{name}'s safety code</span>
          <div className="flex items-center gap-2">
            <code className="flex-1 rounded-md bg-fog-950 px-2 py-1.5 font-mono text-sm tracking-wide text-ink">{theirs}</code>
            <CopyButton text={theirs} />
          </div>
        </div>
        {mine && (
          <div className="space-y-1">
            <span className="field-label">Your safety code, for them to check</span>
            <code className="block rounded-md bg-fog-950 px-2 py-1.5 font-mono text-sm tracking-wide text-ink-muted">{mine}</code>
          </div>
        )}
        {error && <p className="text-warn">{error}</p>}
        <div className="flex justify-end gap-2">
          <button onClick={onClose} className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-hover">
            Not now
          </button>
          <button
            onClick={confirm}
            disabled={busy || !codes?.theirKey}
            className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
            They read the same code
          </button>
        </div>
      </div>
    </Modal>
  );
}
