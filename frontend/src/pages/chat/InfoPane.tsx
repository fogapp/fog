// The room's info pane: who is in it and whether their devices are vouched
// for, the one-sentence account of what the server can and cannot see, and
// the admin verbs. Every line comes from data that has already landed.

import { usePersonLookup } from "../../lib/personLookup";
import { useEffect, useState } from "react";
import { ApiError, type ChatClient, type DirectoryPerson, type ParticipantView, type Room, type RoomDetail, type RoomMember, type Webhook } from "@fog/chat-sdk";
import { errorText } from "../../components/Async";
import CopyButton from "../../components/CopyButton";
import Field, { control } from "../../components/Field";
import { ROLE_NAMES, handleOf, identityKeyFor, nameFor } from "./names";
import VerifyModal from "./VerifyModal";

export default function InfoPane({
  client, room, detail, directory, onClose, onLeft,
}: {
  client: ChatClient;
  room: Room;
  detail: RoomDetail | null;
  directory: DirectoryPerson[];
  onClose: () => void;
  onLeft: () => void;
}) {
  const [label, setLabel] = useState(room.label);
  const [topic, setTopic] = useState(room.topicLabel);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const me = client.me();
  const admin = (room.myRole ?? 0) >= 3;
  const owner = room.myRole === 4;
  // Verification marks live on this device: which participants this person
  // has checked a safety code with, keyed to the identity key they checked.
  const [marks, setMarks] = useState<Record<string, boolean>>({});
  const [verifying, setVerifying] = useState<{ addr: string; name: string } | null>(null);
  const [identityVersion, setIdentityVersion] = useState(0);

  useEffect(() => {
    setLabel(room.label);
    setTopic(room.topicLabel);
  }, [room.id, room.label, room.topicLabel]);

  useEffect(() => {
    const off = client.addEventListener("identity", () => setIdentityVersion((v) => v + 1));
    return () => {
      off();
    };
  }, [client]);

  // What a badge says comes from the group itself: each leaf this device
  // verified against the identity key it holds for that person, and a mark
  // counts only for that same key — never for whatever key fog serves now.
  const [leafTrust, setLeafTrust] = useState<Record<string, "verified" | "unverified">>({});
  // Who the group itself holds, against fog's list: anyone holding a leaf
  // fog doesn't list is unexplained and shown as such.
  const [members, setMembers] = useState<RoomMember[]>([]);
  useEffect(() => {
    let alive = true;
    const parts = detail?.participants.filter((p) => !p.leftSeq) ?? [];
    (async () => {
      const marksNow = Object.fromEntries(
        await Promise.all(parts.map(async (p) => {
          const held = await client.knownIdentity(p.addr, false);
          return [p.addr, !!held && (await client.isVerified(p.addr, held))] as const;
        })),
      );
      const held = await client.members(room.id, parts.map((p) => p.addr));
      const trust: Record<string, "verified" | "unverified"> = {};
      for (const m of held) {
        if (m.devices.some((d) => !d.attested)) trust[m.addr] = "unverified";
        else if (m.devices.length) trust[m.addr] = "verified";
      }
      if (alive) {
        setMarks(marksNow);
        setLeafTrust(trust);
        setMembers(held);
      }
    })().catch(() => {});
    return () => {
      alive = false;
    };
  }, [client, detail, directory, identityVersion, room.id]);

  const act = async (key: string, fn: () => Promise<unknown>, fallback: string) => {
    setBusy(key);
    setError("");
    try {
      await fn();
    } catch (e) {
      setError(errorText(e, fallback));
    } finally {
      setBusy("");
    }
  };

  // devicesFor says whether every leaf the person holds in this room's
  // group is a device their identity key (as this device holds it) attests.
  const devicesFor = (p: ParticipantView): "verified" | "unverified" | "unknown" => leafTrust[p.addr] ?? "unknown";
  const warnings = new Map(client.identityWarnings().map((w) => [w.addr, w.text]));
  // This device's own safety code, from its identity seed — never the key
  // fog serves, which a swapped fog would make agree with itself.
  const [own, setOwn] = useState<{ code: string; swapped: boolean }>({ code: "", swapped: false });
  useEffect(() => {
    let alive = true;
    client.safetyCodes(me.addr).then((c) => alive && setOwn({ code: c.mine ?? "", swapped: c.ownSwapped })).catch(() => {});
    return () => {
      alive = false;
    };
  }, [client, me.addr, identityVersion]);
  const myFingerprint = own.code;

  return (
    <div className="flex h-full flex-col overflow-y-auto">
      <div className="flex h-11 shrink-0 items-center justify-between border-b border-hair px-4">
        <span className="text-sm font-medium text-ink">About this room</span>
        <button onClick={onClose} className="rounded px-1 text-ink-muted hover:text-ink" title="Close" aria-label="Close">
          ✕
        </button>
      </div>
      <div className="space-y-6 px-4 py-4">
        {detail ? (
          <p className="text-hint leading-relaxed text-ink-muted">{detail.impact}</p>
        ) : (
          <p className="text-hint text-ink-muted">Loading…</p>
        )}

        <section className="space-y-2">
          <h3 className="subhead">What the server sees</h3>
          <p className="text-hint leading-relaxed text-ink-muted">
            Who is in the room and from which devices, who sent each message and when, and how large it is. Never the text, reactions,
            threads, or attachments — those are sealed to the devices listed here.
            {room.policy.hints.includes("mentions") ? " Senders may declare who they mentioned so a locked phone can be woken." : ""}
          </p>
        </section>

        <HostingSection key={room.id} client={client} room={room} detail={detail} />

        <section className="space-y-1.5">
          <h3 className="subhead">People · {detail?.participants.filter((p) => !p.leftSeq).length ?? room.participants}</h3>
          {!detail && <p className="text-hint text-ink-muted">Loading…</p>}
          {detail?.participants.filter((p) => !p.leftSeq).map((p) => {
            const v = devicesFor(p);
            const warning = warnings.get(p.addr);
            const key = identityKeyFor(p.addr, directory, client);
            const mine = p.addr === me.addr;
            const marked = !!marks[p.addr];
            const name = nameFor(p.addr, directory, client);
            return (
              <div key={p.addr} className="flex items-center gap-2 text-cell">
                <span className={`h-1.5 w-1.5 rounded-full ${p.online ? "bg-accent" : "bg-fog-700"}`} title={p.online ? "online" : "offline"} />
                <span className="min-w-0 flex-1 truncate text-ink">
                  {name} {mine && <span className="text-ink-muted">(you)</span>}
                </span>
                {!mine && <PersonPages chat={p.addr} />}
                <span className="text-xs text-ink-muted">{ROLE_NAMES[p.roleIndex] ?? p.roleIndex}</span>
                {mine ? (
                  <span className="text-xs text-ink-muted" title={warning ?? (myFingerprint ? `Your safety code: ${myFingerprint}` : "No identity key yet")}>
                    {warning ? <span className="text-warn">⚠ fog serves another identity key for you</span> : v === "unverified" ? <span className="text-warn">a device unverified</span> : "you"}
                  </span>
                ) : warning ? (
                  <span className="text-xs text-warn" title={warning}>
                    {client.awaitingKey(p.addr) ? "⚠ identity reset · new key not accepted" : "⚠ identity key changed"}
                  </span>
                ) : marked && v === "verified" ? (
                  <span className="text-xs text-accent" title="You compared safety codes with this person on this device">
                    🔒 verified
                  </span>
                ) : (
                  <button
                    onClick={() => key && setVerifying({ addr: p.addr, name })}
                    disabled={!key}
                    title={key ? "Compare safety codes to verify this person" : "No identity key known yet"}
                    className="text-xs text-ink-muted hover:text-ink disabled:opacity-50">
                    {v === "unverified" ? <span className="text-warn">device unverified · </span> : ""}not verified · verify
                  </button>
                )}
                {/* Roles move only below your own: an owner sets anyone
                    under owner, up to owner; an admin, members. */}
                {admin && p.addr !== me.addr && (owner ? p.roleIndex < 4 : p.roleIndex < room.myRole) && (
                  <select
                    value={p.roleIndex}
                    onChange={(e) => act("role", () => client.updateRoom(room.id, { role: { addr: p.addr, role: Number(e.target.value) } }), "Couldn't change the role")}
                    className="rounded border border-hair bg-fog-950 px-1 text-xs text-ink-muted"
                    aria-label={`Role for ${handleOf(p.addr)}`}>
                    {[1, 2, 3, 4].filter((r) => r === p.roleIndex || (owner ? true : r < room.myRole)).map((r) => (
                      <option key={r} value={r}>{ROLE_NAMES[r]}</option>
                    ))}
                  </select>
                )}
                {admin && p.addr !== me.addr && (
                  <button
                    onClick={() => act("rm", () => client.removeMember(room.id, p.addr), "Couldn't remove")}
                    disabled={busy === "rm"}
                    className="text-xs text-warn hover:underline">
                    remove
                  </button>
                )}
              </div>
            );
          })}
        </section>

        {/* Leaves fog can't account for: the group says these people can
            read the room, and fog doesn't list them. */}
        {members.some((m) => m.unexplained) && (
          <section className="space-y-1.5" role="alert">
            <h3 className="subhead text-warn">⚠ In the group, not listed by fog</h3>
            <p className="text-hint leading-relaxed text-ink-muted">
              These people hold keys to this room that fog doesn't explain. They can read what is sent here. Don't trust the room until a
              member removes them.
            </p>
            {members.filter((m) => m.unexplained).map((m) => (
              <div key={m.addr} className="flex items-center gap-2 text-cell">
                <span className="min-w-0 flex-1 truncate text-warn">{nameFor(m.addr, directory, client)}</span>
                <span className="text-xs text-ink-muted">
                  {m.devices.length} device{m.devices.length === 1 ? "" : "s"}
                  {m.devices.some((d) => !d.attested) ? " · unverified" : ""}
                </span>
              </div>
            ))}
          </section>
        )}

        {/* People banned from the room. A ban is lifted by the same people
            who may ban (it is a role change), after which a member can add
            them back. */}
        {admin && (detail?.participants.filter((p) => p.leftSeq && p.roleIndex === 1).length ?? 0) > 0 && (
          <section className="space-y-1.5">
            <h3 className="subhead">Banned</h3>
            {detail?.participants.filter((p) => p.leftSeq && p.roleIndex === 1).map((p) => (
              <div key={p.addr} className="flex items-center gap-2 text-cell">
                <span className="min-w-0 flex-1 truncate text-ink-muted">{nameFor(p.addr, directory, client)}</span>
                <button
                  onClick={() => act("unban", () => client.updateRoom(room.id, { role: { addr: p.addr, role: 2 } }), "Couldn't lift the ban")}
                  disabled={busy === "unban"}
                  className="text-xs text-ink-muted hover:text-ink hover:underline">
                  lift ban
                </button>
              </div>
            ))}
            <p className="text-hint text-ink-muted">Lifting a ban doesn't add them back; someone in the room can add them again.</p>
          </section>
        )}

        {admin && room.kind !== "dm" && (
          <section className="space-y-3">
            <h3 className="subhead">Room</h3>
            <Field label="Label" value={label} onChange={(e) => setLabel(e.target.value)} maxLength={64} hint="Visible to the server: it is how the room is listed." />
            <Field label="Topic" value={topic} onChange={(e) => setTopic(e.target.value)} maxLength={64} />
            <button
              onClick={() => act("save", () => client.updateRoom(room.id, { label, topic }), "Couldn't save")}
              disabled={busy === "save" || (label === room.label && topic === room.topicLabel)}
              className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-hover disabled:opacity-50">
              Save
            </button>
          </section>
        )}

        {own.swapped && (
          <p role="alert" className="rounded-md border border-warn/40 bg-warn/10 px-2 py-1.5 text-hint text-warn">
            ⚠ Fog serves a different identity key for you than the one this device holds. Your safety code below is your real one;
            anyone comparing against what fog shows them will not match. Tell your household's operator.
          </p>
        )}
        {myFingerprint && (
          <section className="space-y-1">
            <h3 className="subhead">Your safety code</h3>
            <code className="block rounded-md bg-fog-950 px-2 py-1 font-mono text-xs tracking-wide text-ink-muted">{myFingerprint}</code>
            <p className="text-hint text-ink-muted">What others compare when they verify you. A mark made here stays on this device.</p>
          </section>
        )}

        <section className="space-y-2">
          <h3 className="subhead">Notifications</h3>
          <select
            value={room.notify || "all"}
            onChange={(e) => act("notify", () => client.updateRoom(room.id, { notify: e.target.value }), "Couldn't change")}
            className={control}>
            <option value="all">Everything</option>
            <option value="mentions">Mentions only</option>
            <option value="none">Nothing</option>
          </select>
        </section>

        {admin && room.kind !== "dm" && <WebhooksSection client={client} room={room} act={act} busy={busy} />}

        <section className="space-y-2">
          <h3 className="subhead">Export</h3>
          <p className="text-hint text-ink-muted">Everything this device has decrypted in this room, threads included, as JSON lines. It leaves as a file; the server is not involved.</p>
          <button
            onClick={() => exportRoom(client, room)}
            disabled={room.sync !== "ready"}
            className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink-muted hover:bg-hover disabled:opacity-50">
            Export room
          </button>
        </section>

        <section className="space-y-2">
          <h3 className="subhead">Leave</h3>
          <p className="text-hint text-ink-muted">Leaving keeps what this device has and stops what comes next. The room's keys rotate without you.</p>
          <div className="flex gap-2">
            <button
              onClick={() => act("leave", async () => { await client.leave(room.id); onLeft(); }, "Couldn't leave")}
              disabled={busy === "leave" || room.kind === "dm"}
              className="rounded-md border border-warn/45 px-3 py-1.5 text-sm text-warn hover:bg-warn/10 disabled:opacity-50">
              Leave room
            </button>
            {admin && !room.archivedAt && (
              <button
                onClick={() => act("archive", () => client.updateRoom(room.id, { archive: true }), "Couldn't archive")}
                disabled={busy === "archive"}
                className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink-muted hover:bg-hover disabled:opacity-50">
                Archive
              </button>
            )}
          </div>
        </section>
        {error && <p className="text-hint text-warn">{error}</p>}
      </div>
      {verifying && <VerifyModal client={client} {...verifying} onClose={() => setVerifying(null)} />}
    </div>
  );
}

// HostingSection: which household orders and stores this room's ciphertext.
// Hosting is about sequencing and storage, never about reading — the host
// holds no keys. Admins can hand hosting to another household that already
// has someone in the room. Keyed by room, so switching rooms starts it fresh.
function HostingSection({ client, room, detail }: { client: ChatClient; room: Room; detail: RoomDetail | null }) {
  const me = client.me();
  const [target, setTarget] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState("");
  const [error, setError] = useState("");
  // The server tells local people the room changed and the SDK refreshes it;
  // until that lands, the move we were just told about is the truth.
  const [movedTo, setMovedTo] = useState<string | null>(null);

  useEffect(() => {
    if (movedTo !== null && room.hubInstance === movedTo) setMovedTo(null);
  }, [room.hubInstance, movedTo]);

  const hub = movedTo ?? room.hubInstance;
  const hostedHere = hub === "";
  const households = client.households();
  const householdName = (instanceId: string) => households.find((h) => h.instanceId === instanceId)?.name || "another household";

  const myCaps = room.policy.roles.find((r) => r.index === room.myRole)?.capabilities ?? [];
  const canMove = myCaps.includes("canChangePolicy");
  // Only households with someone currently in the room can take over.
  const present = new Set((detail?.participants ?? []).filter((p) => !p.leftSeq).map((p) => p.homeInstance));
  const candidates = households.filter((h) => h.instanceId !== me.household.instanceId && present.has(h.instanceId));

  // A room comes here only when one of our own people with the right to
  // agrees: the hub can't push it.
  const [accepted, setAccepted] = useState(false);
  const acceptHere = async () => {
    setBusy(true);
    setError("");
    try {
      await client.updateRoom(room.id, { acceptHub: true });
      setAccepted(true);
    } catch (e) {
      setError(errorText(e, "Couldn't agree to host"));
    } finally {
      setBusy(false);
    }
  };

  const move = async () => {
    setBusy(true);
    setError("");
    setResult("");
    try {
      const r = await client.api.migrateRoom(room.id, target);
      setMovedTo(r.hubInstance);
      setResult(`Now hosted by ${householdName(r.hubInstance)}. Everyone's messages continue from #${r.seq}.`);
      setConfirming(false);
      setTarget("");
    } catch (e) {
      if (e instanceof ApiError && e.code === "moving") setError("A move is already in flight; give it a moment.");
      else setError(errorText(e, "Couldn't move hosting"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="space-y-2">
      <h3 className="subhead">Hosting</h3>
      <p className="text-hint leading-relaxed text-ink-muted">
        {hostedHere ? "Hosted here." : `Hosted by ${householdName(hub)}.`} The host puts messages in order and stores their ciphertext; it holds no keys and reads nothing.
      </p>
      {!hostedHere && canMove && room.replicaStatus === "live" && (
        <div className="space-y-1">
          <button onClick={acceptHere} disabled={busy || accepted} className="text-xs text-ink-muted hover:text-ink disabled:opacity-60">
            {accepted ? "You agreed: the host may hand this room to us within the hour" : `Agree to host this room at ${me.household.name || "our household"}`}
          </button>
        </div>
      )}
      {hostedHere && canMove && candidates.length > 0 && !confirming && (
        <div className="flex items-center gap-2">
          <select
            value={target}
            onChange={(e) => setTarget(e.target.value)}
            aria-label="Move hosting to"
            className={control}>
            <option value="">Move hosting to…</option>
            {candidates.map((h) => (
              <option key={h.instanceId} value={h.instanceId}>{h.name}</option>
            ))}
          </select>
          <button
            onClick={() => setConfirming(true)}
            disabled={!target}
            className="rounded-md border border-edge px-2 py-1 text-xs text-ink hover:bg-hover disabled:opacity-50">
            Continue
          </button>
        </div>
      )}
      {confirming && (
        <div className="space-y-2 rounded-md border border-edge p-2">
          <p className="text-hint leading-relaxed text-ink-muted">
            Hosting moves to <span className="text-ink">{householdName(target)}</span>. Posting pauses for a moment while it catches up; attachments are copied
            across; nothing is decrypted anywhere. Members keep their keys and their history.
          </p>
          <div className="flex gap-2">
            <button onClick={move} disabled={busy} className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-hover disabled:opacity-50">
              {busy ? "Moving…" : "Move hosting"}
            </button>
            <button onClick={() => setConfirming(false)} disabled={busy} className="rounded-md px-3 py-1.5 text-sm text-ink-muted hover:text-ink disabled:opacity-50">
              Cancel
            </button>
          </div>
        </div>
      )}
      {result && <p className="text-hint text-ink-muted">{result}</p>}
      {error && <p className="text-hint text-warn">{error}</p>}
    </section>
  );
}

// exportRoom hands the person their room as a .jsonl download, built here
// from the local plaintext store.
function exportRoom(client: ChatClient, room: Room) {
  const body = client.exportRoom(room.id);
  const blob = new Blob([body], { type: "application/x-ndjson" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  const name = (room.label || room.kind).replace(/[^a-z0-9_-]+/gi, "-").replace(/^-+|-+$/g, "") || "room";
  a.href = url;
  a.download = `fog-chat-${name}-${new Date().toISOString().slice(0, 10)}.jsonl`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

// WebhooksSection: incoming webhooks post as fog, so fog must hold a seat in
// the room first. The URL is the secret and is shown exactly once.
function WebhooksSection({
  client, room, act, busy,
}: {
  client: ChatClient;
  room: Room;
  act: (key: string, fn: () => Promise<unknown>, fallback: string) => Promise<void>;
  busy: string;
}) {
  const [hooks, setHooks] = useState<Webhook[] | null>(null);
  const [botInRoom, setBotInRoom] = useState(false);
  const [botAddr, setBotAddr] = useState("");
  const [name, setName] = useState("");
  const [fresh, setFresh] = useState<{ id: number; name: string; url: string } | null>(null);
  const [loadError, setLoadError] = useState("");

  const load = () =>
    client.api
      .webhooks(room.id)
      .then((r) => {
        setHooks(r.webhooks);
        setBotInRoom(r.botInRoom);
        setBotAddr(r.botAddr);
        setLoadError("");
      })
      .catch((e) => setLoadError(errorText(e, "Couldn't load webhooks")));

  useEffect(() => {
    setFresh(null);
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, room.id, room.epoch]);

  return (
    <section className="space-y-2">
      <h3 className="subhead">Webhooks</h3>
      <p className="text-hint text-ink-muted">
        A URL that anything can POST text to; the message appears here as fog. Fog can only post where it is a member, so it needs a seat first.
      </p>
      {!botInRoom && hooks !== null && (
        <div className="flex flex-wrap items-center gap-2 text-hint text-ink-muted">
          <span>Fog isn't in this room.</span>
          {botAddr && (
            <button
              onClick={() => act("addbot", () => client.addMembers(room.id, [botAddr]).then(load), "Couldn't add fog")}
              disabled={busy === "addbot"}
              className="rounded-md border border-edge px-2 py-0.5 text-xs text-ink hover:bg-hover disabled:opacity-50">
              {busy === "addbot" ? "Adding…" : "Add fog to this room"}
            </button>
          )}
        </div>
      )}
      {botInRoom && (
        <form
          className="flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (!name.trim()) return;
            act("hook", async () => {
              const created = await client.api.createWebhook(room.id, name.trim());
              setFresh(created);
              setName("");
              await load();
            }, "Couldn't create the webhook");
          }}>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={40}
            placeholder="name, e.g. grafana"
            aria-label="Webhook name"
            className={`${control} min-w-0 flex-1`}
          />
          <button type="submit" disabled={busy === "hook" || !name.trim()} className="rounded-md border border-edge px-2 py-1 text-xs text-ink hover:bg-hover disabled:opacity-50">
            {busy === "hook" ? "Creating…" : "Create"}
          </button>
        </form>
      )}
      {fresh && (
        <div className="space-y-1 rounded-md border border-edge p-2">
          <p className="text-hint text-warn">
            The URL for <span className="font-mono">{fresh.name}</span> is shown once — copy it now. Anyone holding it can post here as fog.
          </p>
          <div className="flex items-center gap-2">
            <code className="min-w-0 flex-1 truncate rounded bg-fog-950 px-2 py-1 font-mono text-xs text-ink-muted">{fresh.url}</code>
            <CopyButton text={fresh.url} />
          </div>
          <p className="text-hint text-ink-muted">
            Post with <span className="font-mono">{"{\"text\": \"…\"}"}</span> or a plain-text body.
          </p>
        </div>
      )}
      {loadError && <p className="text-hint text-warn">{loadError}</p>}
      {hooks && hooks.length > 0 && (
        <ul className="divide-y divide-divider rounded-md border border-hair">
          {hooks.map((h) => (
            <li key={h.id} className="flex items-center gap-2 px-2 py-1.5 text-cell">
              <span className="min-w-0 flex-1 truncate text-ink">{h.name}</span>
              <span className="text-xs text-ink-muted">{h.lastUsedAt ? `used ${new Date(h.lastUsedAt).toLocaleDateString()}` : "never used"}</span>
              <button
                onClick={() => act("rmhook", async () => { await client.api.deleteWebhook(room.id, h.id); if (fresh?.id === h.id) setFresh(null); await load(); }, "Couldn't delete the webhook")}
                disabled={busy === "rmhook"}
                className="text-xs text-warn hover:underline disabled:opacity-50">
                delete
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

// PersonPages is "Open in Hub" and its kind: where apps say this person
// lives (docs/signin-contract.md).
function PersonPages({ chat }: { chat: string }) {
  const who = usePersonLookup({ chat });
  if (!who.data?.pages.length) return null;
  return (
    <>
      {who.data.pages.map((pg) => (
        <a key={pg.app} href={pg.url} className="text-xs text-accent hover:underline" title={pg.label} rel="noreferrer">{pg.app}</a>
      ))}
    </>
  );
}
