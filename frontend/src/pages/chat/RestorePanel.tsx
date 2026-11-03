// Restoring from a recovery phrase (docs/chat-contract.md): the twelve
// words unlock the backup fog holds but cannot open. Everything this browser
// has is replaced by it. It joins nothing by itself: it leaves its old place
// in each room, and your other devices or a member add it back.

import { useState } from "react";
import { validPhrase } from "@fog/chat-sdk";
import Modal from "../../components/Modal";
import { restoreFromPhrase } from "../../lib/chatClient";
import { useAction } from "../../lib/useAction";
import { controlArea } from "../../components/Field";

export default function RestorePanel({ onClose }: { onClose: () => void }) {
  const [phrase, setPhrase] = useState("");
  const { run, busy, error } = useAction();
  const [done, setDone] = useState<{ addr: string; messages: number } | null>(null);
  const words = phrase.trim().split(/\s+/).filter(Boolean).length;
  const ok = words === 12 && validPhrase(phrase);

  async function restore() {
    await run(async () => {
      setDone(await restoreFromPhrase(phrase));
    }, "Couldn't restore from that phrase");
  }

  return (
    <Modal title="Restore from a recovery phrase" onClose={onClose}>
      {done ? (
        <div className="space-y-3 text-sm text-ink">
          <p>
            Restored <span className="font-mono text-xs text-ink-muted">{done.addr}</span> with {done.messages} messages. This device joins
            no room by itself: your other devices, or a member, add it back to each room.
          </p>
          <button onClick={onClose} className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover">
            Done
          </button>
        </div>
      ) : (
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (ok && !busy) restore();
          }}>
          <p className="text-hint leading-relaxed text-ink-muted">
            Type the twelve words in order. Whatever chat holds on this browser is replaced by the backup: your identity, your devices'
            keys, and the messages you had read.
          </p>
          <textarea
            value={phrase}
            onChange={(e) => setPhrase(e.target.value)}
            rows={3}
            autoFocus
            spellCheck={false}
            autoCapitalize="none"
            placeholder="twelve words, separated by spaces"
            className={`${controlArea} w-full font-mono`}
          />
          <p className="text-hint text-ink-muted">
            {words === 0 ? "" : words !== 12 ? `${words} of 12 words` : ok ? "That reads as a valid phrase." : "Those twelve words do not form a valid phrase — check the spelling."}
          </p>
          {error && <p className="text-sm text-warn">{error}</p>}
          <div className="flex justify-end gap-2">
            <button type="button" onClick={onClose} className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-hover">
              Cancel
            </button>
            <button
              type="submit"
              disabled={!ok || busy}
              className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
              {busy ? "Restoring…" : "Restore"}
            </button>
          </div>
        </form>
      )}
    </Modal>
  );
}
