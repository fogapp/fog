// A peer's settings, gathered in one dialog: the local label, the address
// this side dials, which way crash reports travel, what each side opens to
// the other's apps, and the mirror — what this peer actually receives. The
// detail column stays quiet; the decisions live here.

import { useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, get, post, put, type FedPeer, type FedPeerMirror, type FedRecheck } from "../../api";
import { errorText } from "../../components/Async";
import Button from "../../components/Button";
import Field, { control } from "../../components/Field";
import Modal from "../../components/Modal";
import { SectionHead } from "../../components/Section";
import Toggle from "../../components/Toggle";
import { peerLabel, timeAgo } from "../../lib/format";
import { useApps } from "../../lib/useApps";
import { federationQuery } from "../../queries";

type CapabilityMutation = {
  mutate: (c: { capability: string; direction: string; enabled: boolean; config?: unknown }) => void;
  isPending: boolean;
};

const select = control;

export function PeerSettingsModal({ peer, onClose }: { peer: FedPeer; onClose: () => void }) {
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const refresh = () => qc.invalidateQueries({ queryKey: federationQuery.queryKey });

  const setCapability = useMutation({
    mutationFn: (c: { capability: string; direction: string; enabled: boolean; config?: unknown }) =>
      put(`/api/federation/peers/${peer.id}`, { capabilities: [c] }),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e) => setError(errorText(e, "Couldn't update the peer")),
  });

  const acceptsErrors = peer.capabilities.some((c) => c.capability === "flare.ingest" && c.direction === "grant");
  const sendCap = peer.capabilities.find((c) => c.capability === "flare.ingest" && c.direction === "use");
  const sendMode = sendCap ? String(sendCap.config?.mode ?? "") : "";

  return (
    <Modal title={`${peerLabel(peer)} · settings`} onClose={onClose} width="max-w-xl">
      <div className="space-y-7 text-sm">
        <Part title="Label" hint="Renames the peer on this screen and on the errors they relay. Local: never on their side.">
          <TextSetting
            initial={peer.label ?? ""}
            placeholder={peer.name}
            label="Shown as"
            hint={peer.label ? `They advertise "${peer.name}". Clear it to use that.` : "The name they advertised. Set one to call them something else here."}
            onSave={(label) => put(`/api/federation/peers/${peer.id}`, { label })}
          />
        </Part>

        <Part title="Address" hint="Where this side dials them. While either direction still connects, heartbeats refresh it; edit it when they don't.">
          {peer.dialError && (
            <p className="mb-2 break-words text-hint leading-relaxed text-warn">
              Our dials are failing: {peer.dialError}
              {peer.online && " — they still reach us, so only this address is stale."}
            </p>
          )}
          <TextSetting
            initial={peer.url ?? ""}
            placeholder="https://fog.example.com or http://192.168.4.20:8080"
            label="We dial"
            hint={peer.url ? undefined : "No address stored — they dial us; we can't reach them."}
            onSave={(url) => put(`/api/federation/peers/${peer.id}`, { url })}
          />
        </Part>

        <Part title="Errors" hint="Which way crash reports travel.">
          <Toggle
            checked={acceptsErrors}
            disabled={setCapability.isPending}
            onChange={(on) => setCapability.mutate({ capability: "flare.ingest", direction: "grant", enabled: on })}
            label="Accept their errors into this fog's Inbox"
          />
          <label className="flex items-center justify-between gap-2 text-ink-muted">
            Send ours
            <select
              value={sendMode}
              disabled={setCapability.isPending}
              onChange={(e) =>
                setCapability.mutate(
                  e.target.value === ""
                    ? { capability: "flare.ingest", direction: "use", enabled: false }
                    : { capability: "flare.ingest", direction: "use", enabled: true, config: { mode: e.target.value } },
                )
              }
              className={select}
            >
              <option value="">Off</option>
              <option value="redacted">Redacted</option>
              <option value="full">Full</option>
            </select>
          </label>
        </Part>

        <FeedbackPart peer={peer} setCapability={setCapability} />
        <TheyGrant peer={peer} setCapability={setCapability} />
        <WeShare peer={peer} setCapability={setCapability} />
        <PeoplePart peer={peer} setCapability={setCapability} />
        <ChatPart peer={peer} setCapability={setCapability} />
        <Mirror peer={peer} />
        <Bandwidth peer={peer} />

        {error && <p className="text-hint text-warn">{error}</p>}
      </div>
    </Modal>
  );
}

function Part({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section>
      <SectionHead title={title} />
      {hint && <p className="mt-2 text-hint text-ink-muted">{hint}</p>}
      <div className="mt-3 space-y-2.5">{children}</div>
    </section>
  );
}

// One text setting with its own save: the field shows the stored value,
// the buttons appear once it differs.
function TextSetting({ initial, placeholder, label, hint, onSave }: {
  initial: string;
  placeholder?: string;
  label: string;
  hint?: ReactNode;
  onSave: (v: string) => Promise<unknown>;
}) {
  const qc = useQueryClient();
  const [value, setValue] = useState(initial);
  const [error, setError] = useState("");
  const save = useMutation({
    mutationFn: (v: string) => onSave(v),
    onSuccess: () => { setError(""); return qc.invalidateQueries({ queryKey: federationQuery.queryKey }); },
    onError: (e) => setError(errorText(e, "Couldn't save")),
  });
  const dirty = value.trim() !== initial;
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate(value.trim());
      }}
    >
      <Field label={label} value={value} onChange={(e) => setValue(e.target.value)} placeholder={placeholder} maxLength={label === "Shown as" ? 64 : undefined}
        hint={error || hint} aria-invalid={!!error} />
      {dirty && (
        <div className="mt-2 flex justify-end gap-2">
          <Button size="sm" variant="ghost" type="button" onClick={() => setValue(initial)}>Cancel</Button>
          <Button size="sm" variant="primary" type="submit" disabled={save.isPending}>{save.isPending ? "Saving…" : "Save"}</Button>
        </div>
      )}
    </form>
  );
}

// Feedback from the header's bug button, both ways. Receiving files their
// reports in this Inbox; sending chooses how much of ours crosses — the
// words and the page, or those with the dossier (fogd's logs, open errors
// and issues, recent activity, what the browser saw).
function FeedbackPart({ peer, setCapability }: { peer: FedPeer; setCapability: CapabilityMutation }) {
  const accepts = peer.capabilities.some((c) => c.capability === "feedback.ingest" && c.direction === "grant");
  const sendCap = peer.capabilities.find((c) => c.capability === "feedback.ingest" && c.direction === "use");
  const sendMode = sendCap ? String(sendCap.config?.mode ?? "") : "";
  const theyAccept = peer.remoteGrants.includes("feedback.ingest");
  return (
    <Part title="Feedback" hint="What people write from the bug button in the header.">
      <Toggle
        checked={accepts}
        disabled={setCapability.isPending}
        onChange={(on) => setCapability.mutate({ capability: "feedback.ingest", direction: "grant", enabled: on })}
        label="Accept their feedback into this fog's Inbox"
      />
      <label className="flex items-center justify-between gap-2 text-ink-muted">
        Send ours
        <select
          value={sendMode}
          disabled={setCapability.isPending}
          onChange={(e) =>
            setCapability.mutate(
              e.target.value === ""
                ? { capability: "feedback.ingest", direction: "use", enabled: false }
                : { capability: "feedback.ingest", direction: "use", enabled: true, config: { mode: e.target.value } },
            )
          }
          className={select}
        >
          <option value="">Off</option>
          <option value="note">Words and page</option>
          <option value="full">With the dossier</option>
        </select>
      </label>
      {sendMode === "full" && (
        <p className="text-hint text-ink-muted">
          The dossier carries fogd's recent logs, open errors and issues, recent activity, and what the browser saw.
        </p>
      )}
      {sendMode && !theyAccept && (
        <p className="text-hint text-warn">
          Their last heartbeat didn't accept feedback from this fog, so reports wait here — and keep retrying — until they turn it on.
        </p>
      )}
    </Part>
  );
}

// What the far side has opened to us, and what this instance does with
// it. The chips are their claim, refreshed by every heartbeat — this side
// cannot change them, only decide whether to use them.
function TheyGrant({ peer, setCapability }: { peer: FedPeer; setCapability: CapabilityMutation }) {
  const visible = peer.capabilities.some((c) => c.capability === "apps.directory" && c.direction === "use");
  return (
    <Part title="They grant" hint="What their fog has opened to this one, as their last heartbeat said it.">
      {peer.remoteGrants.length > 0 ? (
        <span className="flex flex-wrap items-center gap-1.5 text-hint text-ink-muted">
          they grant:
          {peer.remoteGrants.map((g) => (
            <span key={g} className="rounded-full border border-edge px-2 py-0.5 text-ink-muted">{g}</span>
          ))}
        </span>
      ) : (
        <p className="text-hint text-ink-muted">
          Nothing yet — grants arrive on their heartbeats, so a peer we have not heard from since pairing shows none.
        </p>
      )}
      <Toggle
        checked={visible}
        disabled={setCapability.isPending}
        onChange={(on) => setCapability.mutate({ capability: "apps.directory", direction: "use", enabled: on })}
        label="Show their apps to my apps"
      />
      {peer.directory.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 text-hint text-ink-muted">
          their apps:
          {peer.directory.map((a) => (
            <a key={a.app} href={a.url || undefined} target="_blank" rel="noreferrer"
              className="rounded-full border border-edge px-2 py-0.5 text-ink-muted hover:border-fog-500 hover:text-ink">
              {a.title || a.app}
            </a>
          ))}
        </div>
      )}
    </Part>
  );
}

// Who is in this household, as the peer sees it (people.directory,
// docs/signin-contract.md): names, handles and person addresses —
// never email, sessions, or which apps anyone uses. Shared with every new
// pairing; chat needs it, since nobody can be reached who is not known.
function PeoplePart({ peer, setCapability }: { peer: FedPeer; setCapability: CapabilityMutation }) {
  const grant = peer.capabilities.find((c) => c.capability === "people.directory" && c.direction === "grant");
  const chatOn = peer.capabilities.some((c) => c.capability === "chat" && c.direction === "grant");
  return (
    <Part title="People" hint="Who lives here, as their fog sees it. Never email, sessions, or which apps anyone uses.">
      <Toggle
        checked={!!grant}
        disabled={setCapability.isPending}
        onChange={(on) => setCapability.mutate({ capability: "people.directory", direction: "grant", enabled: on, config: on ? { people: "members" } : undefined })}
        label={`Let ${peerLabel(peer)} see who is in this household`}
      />
      {!grant && chatOn && (
        <p className="text-hint text-warn">Chat is on, but nobody here can be reached from {peerLabel(peer)} without this.</p>
      )}
      {!peer.personAddresses && (
        <p className="text-hint text-ink-muted">
          {peerLabel(peer)}'s fog doesn't send person addresses yet, so chat with them keeps using handles. It will once they update.
        </p>
      )}
      <VisitToggle peer={peer} setCapability={setCapability} directory={!!grant} />
    </Part>
  );
}

// Visiting (people.visit, docs/signin-contract.md): this household's
// people may sign in to the peer's apps as visitors, on this fog's word.
// The peer still chooses which apps, and as what; this only lets it ask.
function VisitToggle({ peer, setCapability, directory }: { peer: FedPeer; setCapability: CapabilityMutation; directory: boolean }) {
  const grant = peer.capabilities.find((c) => c.capability === "people.visit" && c.direction === "grant");
  return (
    <>
      <Toggle
        checked={!!grant}
        disabled={setCapability.isPending || (!grant && !directory)}
        onChange={(on) => setCapability.mutate({ capability: "people.visit", direction: "grant", enabled: on, config: on ? { people: "members" } : undefined })}
        label={`Let my people sign in to ${peerLabel(peer)}'s apps as visitors`}
      />
      <p className="text-hint text-ink-muted">
        {peerLabel(peer)} decides which of its apps, and what a visitor may do there. Switching this off signs everyone out of them.
      </p>
    </>
  );
}

// Chat across households (docs/chat-contract.md). The grant lets their
// people share rooms with ours — and their fog fetch our key material and
// follow rooms hubbed here; the third-household switch is the one refusal a
// follower can make. Both sides must grant before a room can span.
function ChatPart({ peer, setCapability }: { peer: FedPeer; setCapability: CapabilityMutation }) {
  const grant = peer.capabilities.find((c) => c.capability === "chat" && c.direction === "grant");
  const accepts = grant ? grant.config?.acceptThirdHouseholds !== false : true;
  const theyGrant = peer.remoteGrants.includes("chat");
  return (
    <Part title="Chat" hint="Rooms are end-to-end encrypted: neither fog can read them. What crosses is who is in a room, and ciphertext.">
      <Toggle
        checked={!!grant}
        disabled={setCapability.isPending}
        onChange={(on) => setCapability.mutate({ capability: "chat", direction: "grant", enabled: on, config: on ? { acceptThirdHouseholds: true } : undefined })}
        label={`Let ${peerLabel(peer)}'s people share rooms with mine`}
      />
      {grant && (
        <Toggle
          checked={accepts}
          disabled={setCapability.isPending}
          onChange={(on) => setCapability.mutate({ capability: "chat", direction: "grant", enabled: true, config: { acceptThirdHouseholds: on } })}
          label="Accept their rooms that include households I am not paired with"
        />
      )}
      <p className="text-hint text-ink-muted">
        {theyGrant
          ? "They grant chat to this household too, so rooms can span both."
          : "They have not granted chat back yet; until they do, their people cannot be added here and ours cannot be added there."}
      </p>
    </Part>
  );
}

// The grants this side makes: publishing my routed apps in heartbeats
// toward this peer (all, or a checklist), and whether their apps may ask
// mine to connect.
function WeShare({ peer, setCapability }: { peer: FedPeer; setCapability: CapabilityMutation }) {
  const shareCap = peer.capabilities.find((c) => c.capability === "apps.directory" && c.direction === "grant");
  const sharedApps = (shareCap?.config?.apps as string[] | undefined) ?? [];
  // {} shares every app; {apps: [...]} exactly those — an empty list is
  // "selected, none yet", never "all".
  const shareState = !shareCap ? "off" : Array.isArray(shareCap.config?.apps) ? "selected" : "all";
  const introduces = peer.capabilities.some((c) => c.capability === "apps.introduce" && c.direction === "grant");
  const qc = useQueryClient();
  const apps = useApps({ staleTime: 30_000, enabled: shareState !== "off" });

  // Derived from the freshest capability row in the cache at the moment
  // of the click, never from the sharedApps this render closed over: two
  // quick toggles would otherwise race and silently un-share one.
  const latestShared = (): string[] => {
    const info = qc.getQueryData(federationQuery.queryKey);
    const p = info?.peers.find((x) => x.id === peer.id) ?? peer;
    const cap = p.capabilities.find((c) => c.capability === "apps.directory" && c.direction === "grant");
    return (cap?.config?.apps as string[] | undefined) ?? [];
  };
  const setShare = (state: string) => {
    if (state === "off") setCapability.mutate({ capability: "apps.directory", direction: "grant", enabled: false });
    else setCapability.mutate({ capability: "apps.directory", direction: "grant", enabled: true, config: state === "all" ? {} : { apps: latestShared() } });
  };
  const toggleApp = (name: string, share: boolean) => {
    const cur = latestShared();
    const next = share ? (cur.includes(name) ? cur : [...cur, name]) : cur.filter((n) => n !== name);
    setCapability.mutate({ capability: "apps.directory", direction: "grant", enabled: true, config: { apps: next } });
  };

  return (
    <Part title="We share" hint="What this fog publishes toward them in its heartbeats.">
      <label className="flex items-center justify-between gap-2 text-ink-muted">
        Share my apps
        <select value={shareState} disabled={setCapability.isPending} onChange={(e) => setShare(e.target.value)} className={select}>
          <option value="off">Off</option>
          <option value="all">All routed apps</option>
          <option value="selected">Selected…</option>
        </select>
      </label>
      {shareState === "selected" && apps.data && (
        <div className="flex flex-wrap gap-x-4 gap-y-1 text-hint text-ink-muted">
          {apps.data.map((a) => (
            <label key={a.name} className="flex items-center gap-1.5">
              <input type="checkbox" checked={sharedApps.includes(a.name)} disabled={setCapability.isPending}
                onChange={(e) => toggleApp(a.name, e.target.checked)} />
              {a.name}
            </label>
          ))}
        </div>
      )}
      <Toggle
        checked={introduces}
        disabled={setCapability.isPending}
        onChange={(on) => setCapability.mutate({ capability: "apps.introduce", direction: "grant", enabled: on })}
        label="Allow its apps to connect to mine"
      />
    </Part>
  );
}

// The mirror: step into this peer's shoes and see exactly what it
// receives from us. It grants nothing and sends nothing; it reflects
// decisions already made.
function Mirror({ peer }: { peer: FedPeer }) {
  const mirror = useQuery({
    queryKey: ["federation", "mirror", peer.id],
    queryFn: () => get<FedPeerMirror>(`/api/federation/peers/${peer.id}/mirror`),
  });
  return (
    <Part title="What they see">
      {mirror.isLoading && <p className="text-hint text-ink-muted">Looking through their eyes…</p>}
      {mirror.isError && <p className="text-hint text-warn">Couldn't build the view.</p>}
      {mirror.data && (
        <div className="space-y-2">
          <p className="leading-relaxed text-ink">{mirror.data.sentence}</p>
          <ul className="space-y-1">
            {mirror.data.capabilities.map((c) => (
              <li key={c.capability} className="flex items-baseline gap-2 text-hint">
                <span className={c.granted ? "text-accent" : "text-ink-faint"}>{c.granted ? "✓" : "—"}</span>
                <span className={c.granted ? "text-ink" : "text-ink-muted line-through"}>{c.label}</span>
              </li>
            ))}
          </ul>
          {mirror.data.directory.length > 0 && (
            <p className="text-hint text-ink-muted">Apps offered: {mirror.data.directory.map((d) => d.title || d.app).join(", ")}</p>
          )}
        </div>
      )}
    </Part>
  );
}

// The bandwidth cap for transfers from this peer: the files.share use
// row's config, so it lives with the consent it qualifies.
function Bandwidth({ peer }: { peer: FedPeer }) {
  const qc = useQueryClient();
  const useCap = peer.capabilities.find((c) => c.capability === "files.share" && c.direction === "use");
  const capBps = Number((useCap?.config as { bandwidthBps?: number } | undefined)?.bandwidthBps ?? 0);
  const [text, setText] = useState<string | null>(null);
  const [error, setError] = useState("");
  const setCap = useMutation({
    mutationFn: (bandwidthBps: number) =>
      put(`/api/federation/peers/${peer.id}`, { capabilities: [{ capability: "files.share", direction: "use", enabled: true, config: { bandwidthBps } }] }),
    onSuccess: () => { setText(null); setError(""); return qc.invalidateQueries({ queryKey: federationQuery.queryKey }); },
    onError: (e) => setError(errorText(e, "Couldn't set the cap")),
  });
  const commit = () => {
    if (text === null) return;
    const mb = parseFloat(text);
    const bps = Number.isFinite(mb) && mb > 0 ? Math.round(mb * 1_000_000) : 0;
    if (bps !== capBps) setCap.mutate(bps);
    else setText(null);
  };
  if (!peer.remoteGrants.includes("files.share") && capBps === 0) return null;
  return (
    <Part title="Drive" hint="How fast their folders may arrive here.">
      <p className="flex items-center gap-1.5 text-hint text-ink-muted">
        Transfers from {peerLabel(peer)}: at most
        <input
          type="text"
          inputMode="decimal"
          value={text ?? (capBps > 0 ? String(capBps / 1_000_000) : "")}
          placeholder="∞"
          onChange={(e) => setText(e.target.value)}
          onBlur={commit}
          onKeyDown={(e) => { if (e.key === "Enter") commit(); }}
          className="w-14 rounded border border-edge bg-fog-950 px-1 py-0.5 text-right text-ink"
          aria-label={`Bandwidth cap for transfers from ${peerLabel(peer)}, in MB/s`}
        />
        MB/s
      </p>
      {error && <p className="text-hint text-warn">{error}</p>}
    </Part>
  );
}

// UnpairedModal: the situation stated plainly, and the two things an
// operator can actually do about it. No "dismiss": the stamp is a fact
// about the peer, and the way to disagree with it is to test it.
export function UnpairedModal({ peer, onClose, onRemoved }: { peer: FedPeer; onClose: () => void; onRemoved: () => void }) {
  const qc = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState("");
  const name = peerLabel(peer);
  const recheck = useMutation({
    mutationFn: () => post<FedRecheck>(`/api/federation/peers/${peer.id}/recheck`),
    onSuccess: () => { setError(""); return qc.invalidateQueries({ queryKey: federationQuery.queryKey }); },
    onError: (e) => setError(errorText(e, "Couldn't run the check")),
  });
  const remove = useMutation({
    mutationFn: () => del(`/api/federation/peers/${peer.id}`),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: federationQuery.queryKey });
      onRemoved();
    },
    onError: (e) => setError(errorText(e, "Couldn't remove the peer")),
  });

  return (
    <Modal title={`${name} unpaired this instance`} onClose={onClose} width="max-w-xl">
      <div className="space-y-4 text-sm text-ink-muted">
        <AboutPart title="What happened">
          Their fog has answered this instance's last {peer.rejectStreak ?? 0} signed heartbeats with "no record of
          this instance" — what a fog says once its operator has removed a peer.
          {peer.unpairedAt ? ` We concluded it ${timeAgo(peer.unpairedAt)}.` : ""} Connect pushes nothing when
          someone unpairs, so a run of refusals is the whole notice.
        </AboutPart>
        <AboutPart title="What it means now">
          They accept nothing further from here — errors this fleet sends, directory data, connections. Anything
          that was flowing toward them has already stopped.
        </AboutPart>
        <AboutPart title="Their way in is still open">
          Unpairing only closes the door it was pressed on. Their key is still trusted here, so {name} can still
          reach this instance and use everything it was granted. Removing them below is what closes this side.
        </AboutPart>
        <AboutPart title="Reconnecting">
          Ask them for a fresh invite code and use Join with a code. Pairing again refreshes this entry in place,
          keeping its label and settings.
        </AboutPart>
        {recheck.data && (
          <p className={`rounded-lg border px-3 py-2 ${recheck.data.ok ? "border-accent/30 bg-accent/10 text-accent" : "border-edge bg-fog-950/60 text-ink-muted"}`}>
            {recheck.data.ok
              ? "They answered. The pairing is live again — this state has cleared itself."
              : recheck.data.unpaired
                ? "They still have no record of this instance."
                : `Couldn't reach them, which is a different problem: ${recheck.data.error ?? "no answer"}`}
          </p>
        )}
        <div className="flex flex-wrap items-center gap-3 border-t border-hair pt-4">
          <Button onClick={() => recheck.mutate()} disabled={recheck.isPending || remove.isPending}>
            {recheck.isPending ? "Dialling…" : "Check again"}
          </Button>
          {confirming ? (
            <>
              <Button variant="danger" onClick={() => remove.mutate()} disabled={remove.isPending}>
                {remove.isPending ? "Removing…" : `Yes, remove ${name}`}
              </Button>
              <button type="button" onClick={() => setConfirming(false)} className="text-xs text-ink-muted hover:text-ink">Keep them</button>
            </>
          ) : (
            <button type="button" onClick={() => setConfirming(true)} className="text-sm text-warn hover:text-warn">Remove {name}…</button>
          )}
        </div>
        {confirming && (
          <p className="text-hint text-warn">
            This closes their access, deletes every error group they relayed, and asks contracted apps to revoke
            credentials minted for them. It cannot be undone.
          </p>
        )}
        {error && <p className="text-hint text-warn">{error}</p>}
      </div>
    </Modal>
  );
}

// AboutModal: the whole page explained in one place, so no individual
// control has to carry a paragraph of its own.
export function AboutModal({ onClose }: { onClose: () => void }) {
  return (
    <Modal title="How Connect works" onClose={onClose} width="max-w-xl">
      <div className="space-y-4 text-sm text-ink-muted">
        <AboutPart title="Pairing">
          One code, one paste, both sides paired — keys exchange automatically and no token is ever shown. Codes
          are single-use and expire after 24 hours. Generate one for the other operator, or paste theirs into
          Join with a code. A paired household's key never changes: a pairing that claims to be a household
          already here, under another key, is refused and shows up as a notice. A household that reinstalled
          from scratch is a new household — remove its old entry, then pair again.
        </AboutPart>
        <AboutPart title="What a peer may do">
          Every setting on a peer is enforced here, on every request — what a peer claims about itself never
          grants it anything. Removing a peer ends their access immediately and deletes every error group they sent.
        </AboutPart>
        <AboutPart title="When a pairing ends">
          Unpairing is one-sided and silent — it closes the door it was pressed on, and nothing is sent to the
          other instance. Each side finds out because its own heartbeats start coming back refused, so the state
          takes a few minutes to appear. A peer that has unpaired this instance turns amber here.
        </AboutPart>
        <AboutPart title="Errors">
          Accepting a peer's errors lets it report its crashes into this fog. Sending ours pushes this fleet's
          errors to them, redacted or in full.
        </AboutPart>
        <AboutPart title="Feedback">
          Accepting a peer's feedback files what their people write from the bug button into this fog's Inbox.
          Sending ours delivers each report as it is written — the words and the page, or those with the dossier
          fog attaches — and the report shows whether it arrived. Turning sending off stops anything still waiting.
        </AboutPart>
        <AboutPart title="Apps">
          Sharing publishes this instance's routed apps — all of them, or a chosen few — in the heartbeats it sends
          that peer. Showing theirs exposes their directory to apps running here, and connecting lets their apps
          ask to talk to mine.
        </AboutPart>
        <AboutPart title="Drive">
          A folder is shared with one peer through a lens — what crosses, how much of each file, from when,
          whether they may add files, whether their apps may mount it — and the composer reads the lens back as
          one sentence before anything leaves. The peer subscribes and keeps a read-only copy under Drive → Peers,
          refreshed on every sync; a copy is never re-shared, and when a share ends the copy stays until its owner
          decides.
        </AboutPart>
        <AboutPart title="The link">
          Every two minutes this fog dials each peer it has an address for. The round trip of the last answered
          dial, and how many of the recent dials were answered at all, are the link figures on the Peers tab —
          measured here, never reported by the peer.
        </AboutPart>
      </div>
    </Modal>
  );
}

function AboutPart({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="space-y-1">
      <h3 className="text-sm font-medium text-ink">{title}</h3>
      <p className="leading-relaxed text-ink-muted">{children}</p>
    </section>
  );
}
