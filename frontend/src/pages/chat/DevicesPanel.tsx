// This person's devices: which are vouched for by their identity key, which
// one is this, and the fingerprint someone can read back to them. A device
// that holds the identity key can vouch for the others. The recovery phrase
// lives here too — the one thing that brings a lost phone's words back.

import { useEffect, useState } from "react";
import { fingerprint, unb64, type ChatClient, type DeviceView, type MeResponse, type SessionView } from "@fog/chat-sdk";
import Modal from "../../components/Modal";
import CopyButton from "../../components/CopyButton";
import { errorText } from "../../components/Async";
import SessionList from "../../components/SessionList";
import { refreshOtherDevices } from "../../lib/chatClient";

export default function DevicesPanel({ client, onClose, onRestore }: { client: ChatClient; onClose: () => void; onRestore?: () => void }) {
  const [server, setServer] = useState<MeResponse | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<number | string>(0);
  const [phrase, setPhrase] = useState<string | null>(null);
  const [sessions, setSessions] = useState<{ sessions: SessionView[]; current: number } | null>(null);
  const me = client.me();

  const load = () =>
    Promise.all([
      client.api.me().then(setServer),
      client.api.sessions().then(setSessions),
    ]).catch((e) => setError(errorText(e, "Couldn't load devices")));
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const act = async (key: number | string, fn: () => Promise<unknown>, fallback: string) => {
    setBusy(key);
    setError("");
    try {
      await fn();
      await load();
      await refreshOtherDevices();
    } catch (e) {
      setError(errorText(e, fallback));
    } finally {
      setBusy(0);
    }
  };

  const devices: DeviceView[] | null = server?.devices ?? null;
  const others = devices?.filter((d) => d.deviceKey !== me.deviceKey) ?? [];
  const fp = fingerprint(unb64(me.deviceKey));

  return (
    <Modal title="Your devices" onClose={onClose}>
      <div className="space-y-5">
        {!me.verified && others.length > 0 && (
          <div className="rounded-md border border-warn/40 px-3 py-2 text-cell text-warn">
            This device isn't verified. Open chat on your other device and approve it under Devices — rooms appear here once it does.
            {onRestore && (
              <>
                {" "}
                Lost that device?{" "}
                <button onClick={onRestore} className="underline hover:text-ink">
                  Restore from your recovery phrase
                </button>
                .
              </>
            )}
          </div>
        )}

        <p className="text-hint leading-relaxed text-ink-muted">
          Each device holds its own keys. One that your identity key has vouched for shows as verified everywhere; one that hasn't
          shows as unverified until you approve it from a verified device.
        </p>

        <div className="space-y-1">
          <span className="subhead">This device's fingerprint</span>
          <div className="flex items-center gap-2">
            <code className="flex-1 rounded-md bg-fog-950 px-2 py-1 font-mono text-xs text-ink">{fp}</code>
            <CopyButton text={fp} />
          </div>
          <p className="text-hint text-ink-muted">
            {me.verified ? "Verified by your identity key." : "Not yet vouched for."}
            {me.hasIdentitySeed ? " This device holds your identity key and can approve the others." : ""}
          </p>
        </div>

        {!devices && !error && <p className="text-hint text-ink-muted">Loading…</p>}
        {devices && (
          <ul className="divide-y divide-divider rounded-md border border-hair">
            {devices.map((d) => {
              const dfp = fingerprint(unb64(d.deviceKey));
              const approvable = !d.verified && me.hasIdentitySeed && d.deviceKey !== me.deviceKey;
              return (
                <li key={d.id} className="px-3 py-2 text-cell">
                  <div className="flex items-center gap-3">
                    <div className="min-w-0 flex-1">
                      <div className="text-ink">
                        {d.name} {d.deviceKey === me.deviceKey && <span className="text-ink-muted">(this one)</span>}
                      </div>
                      <div className="text-xs text-ink-muted">
                        {d.kind} · {d.verified ? <span className="text-accent">verified</span> : <span className="text-warn">unverified</span>}
                        {d.lastSeenAt ? ` · seen ${new Date(d.lastSeenAt).toLocaleDateString()}` : ""} · {d.keyPackages} key packages
                      </div>
                    </div>
                    {approvable && (
                      <button
                        onClick={() =>
                          act(d.id, async () => {
                            await client.attestOtherDevice(d.id, d.deviceKey);
                            await client.syncSiblingDevices();
                          }, "Couldn't approve")
                        }
                        disabled={busy === d.id}
                        className="rounded-md border border-accent/30 px-2 py-1 text-xs text-accent hover:bg-accent/10 disabled:opacity-50">
                        {busy === d.id ? "Approving…" : "Approve"}
                      </button>
                    )}
                    {d.deviceKey !== me.deviceKey && (
                      <button
                        onClick={() =>
                          act(`rm${d.id}`, async () => {
                            await client.api.revokeDevice(d.id);
                            await client.syncSiblingDevices();
                          }, "Couldn't revoke")
                        }
                        disabled={busy === `rm${d.id}`}
                        className="rounded-md border border-warn/30 px-2 py-1 text-xs text-warn hover:bg-warn/10 disabled:opacity-50">
                        Revoke
                      </button>
                    )}
                  </div>
                  {approvable && (
                    <p className="mt-1 text-hint text-ink-muted">
                      Approve only if that device shows this fingerprint: <code className="font-mono text-ink-muted">{dfp}</code>
                    </p>
                  )}
                </li>
              );
            })}
          </ul>
        )}

        <section className="space-y-2 border-t border-hair pt-4">
          <span className="subhead">Signed-in sessions</span>
          <p className="text-hint text-ink-muted">
            Every browser and app currently signed in as you. Ending one signs that client out; the device and its keys stay until you
            revoke the device above.
          </p>
          {!sessions && !error && <p className="text-hint text-ink-muted">Loading…</p>}
          {sessions && (
            <SessionList
              sessions={sessions.sessions}
              current={sessions.current}
              busy={typeof busy === "string" && busy.startsWith("sess") ? Number(busy.slice(4)) : undefined}
              onEnd={(id) => act(`sess${id}`, () => client.api.revokeSession(id), "Couldn't end that session")}
            />
          )}
        </section>

        <RecoverySection client={client} server={server} phrase={phrase} setPhrase={setPhrase} busy={busy} act={act} />

        {error && <p className="text-sm text-warn">{error}</p>}
      </div>
    </Modal>
  );
}

// The recovery phrase: twelve words shown exactly once. Fog stores the
// backup they unlock but cannot open it, so it cannot show the words again
// and cannot bring a conversation back without them.
function RecoverySection({
  client, server, phrase, setPhrase, busy, act,
}: {
  client: ChatClient;
  server: MeResponse | null;
  phrase: string | null;
  setPhrase: (p: string | null) => void;
  busy: number | string;
  act: (key: number | string, fn: () => Promise<unknown>, fallback: string) => Promise<void>;
}) {
  const on = client.backupEnabled();
  const saved = server?.backup?.updatedAt;
  return (
    <section className="space-y-2 border-t border-hair pt-4">
      <span className="subhead">Recovery phrase</span>
      {phrase ? (
        <div className="space-y-2 rounded-md border border-warn/40 p-3">
          <p className="text-cell text-warn">
            Write these twelve words down and keep them somewhere safe. Fog cannot show them again, and without them nobody — not
            you, not fog — can bring your conversations back to a new device.
          </p>
          <ol className="grid grid-cols-3 gap-x-4 gap-y-1 font-mono text-sm text-ink sm:grid-cols-4">
            {phrase.split(" ").map((w, i) => (
              <li key={i} className="tabular-nums">
                <span className="mr-1.5 text-ink-muted">{i + 1}.</span>
                {w}
              </li>
            ))}
          </ol>
          <div className="flex items-center gap-2">
            <CopyButton text={phrase} />
            <button
              onClick={() => setPhrase(null)}
              className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover">
              I wrote it down
            </button>
          </div>
        </div>
      ) : on ? (
        <div className="flex items-center gap-3">
          <p className="flex-1 text-hint text-ink-muted">
            Backup is on{saved ? ` · last saved ${new Date(saved).toLocaleString()}` : " · saving…"}. It holds your identity, this
            device's keys, and the messages you have read, sealed under your phrase.
          </p>
          <button
            onClick={() => act("backup-off", () => client.disableBackup(), "Couldn't turn the backup off")}
            disabled={busy === "backup-off"}
            className="rounded-md border border-warn/30 px-2 py-1 text-xs text-warn hover:bg-warn/10 disabled:opacity-50">
            Turn off
          </button>
        </div>
      ) : (
        <div className="flex items-center gap-3">
          <p className="flex-1 text-hint text-ink-muted">
            No backup yet. Losing every device without one loses your conversations — that is the cost of nobody else holding the key.
          </p>
          <button
            onClick={() => act("backup-on", async () => setPhrase(await client.enableBackup()), "Couldn't set up the phrase")}
            disabled={busy === "backup-on"}
            className="rounded-md border border-accent/30 px-2 py-1 text-xs text-accent hover:bg-accent/10 disabled:opacity-50">
            {busy === "backup-on" ? "Setting up…" : "Set up a recovery phrase"}
          </button>
        </div>
      )}
    </section>
  );
}
