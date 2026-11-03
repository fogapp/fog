// The Peers tab: one picture, then one table. The map puts this fog at the
// centre with its peers around it and is for orientation; the table below
// holds the same peers as rows and is for work. They share one selection
// with the sidebar and the detail column, so choosing a peer anywhere
// chooses it everywhere.

import type { FedPeer, FederationInfo, FileFedShare, FileFedSubscription } from "../../api";
import Button from "../../components/Button";
import { peerLabel, plural, timeAgo, versionLabel } from "../../lib/format";
import { Bar, Dot, Group, Head, Pill, Row, Table } from "../app/cockpit";
import { peerState, peerTone, rttText, seenText, stateWord } from "./data";
import PeerMap from "./PeerMap";


export interface PeerFacts {
  peer: FedPeer;
  // Folders we share with them (they hold copies for us), and folders of
  // theirs we hold copies of.
  holds: FileFedShare[];
  shares: FileFedSubscription[];
}

const COLS = "minmax(150px,1.3fr) 96px 96px 1fr 1fr 110px";

export function PeersTab({ info, facts, selected, onSelect, onSelf }: {
  info: FederationInfo;
  facts: PeerFacts[];
  selected: number | null;
  onSelect: (id: number | null) => void;
  onSelf: () => void;
}) {
  const peers = facts.map((f) => f.peer);
  const reachable = peers.filter((p) => peerState(p) === "ok").length;
  const toggle = (id: number) => onSelect(selected === id ? null : id);

  return (
    <div>
      <PeerMap identity={info.identity} facts={facts} selected={selected} onSelect={onSelect} onSelf={onSelf} />

      <Group title="Peers" right={<span className="text-hint text-ink-muted">{peers.length === 0 ? "none yet" : `${reachable} of ${peers.length} reachable`}</span>}>
        {peers.length === 0 ? (
          <p className="text-hint text-ink-muted">
            No peers yet. Invite one with a code, or join another fog with the code they gave you.
          </p>
        ) : (
          <Table>
            <Head cols={COLS}>
              <span>Peer</span><span>Version</span><span>Link</span><span>Holds for us</span><span>Shares with us</span><span className="text-right">Seen</span>
            </Head>
            {facts.map(({ peer: p, holds, shares }) => {
              const state = peerState(p);
              const off = state !== "ok";
              return (
                <Row key={p.id} cols={COLS} tone={selected === p.id ? "sel" : off ? "warn" : undefined} onClick={() => toggle(p.id)}>
                  <span className={`inline-flex min-w-0 items-center gap-[9px] ${off ? "text-ink-muted" : "text-ink"}`}>
                    <Dot tone={peerTone(p)} title={stateWord(p)} />
                    <span className="truncate">{peerLabel(p)}</span>
                  </span>
                  <span className="font-mono text-ink-muted">{p.fogVersion ? versionLabel(p.fogVersion) : "—"}</span>
                  <LinkCell p={p} />
                  <span className="truncate text-ink-muted" title={holds.map((h) => h.path).join("\n")}>{holds.length ? holds.map((h) => h.title).join(", ") : "—"}</span>
                  <span className="truncate font-mono text-ink-muted" title={shares.map((s) => s.folder).join("\n")}>{shares.length ? shares.map((s) => s.title).join(", ") : "—"}</span>
                  <span className={`text-right text-hint ${off ? "text-warn" : "text-ink-muted"}`}>{seenText(p)}</span>
                </Row>
              );
            })}
          </Table>
        )}
      </Group>
    </div>
  );
}

// The Link column: the last round trip our heartbeat measured, or why
// there is none — a peer with no address is only ever dialled by, and one
// whose dials fail has no answer to time.
function LinkCell({ p }: { p: FedPeer }) {
  if (p.unpairedAt) return <span className="text-hint text-warn">unpaired</span>;
  if (p.dialError) return <span className="text-hint text-warn">no answer</span>;
  if (!p.url) return <span className="text-hint text-ink-muted">they dial us</span>;
  if (p.rttMs === undefined) return <span className="font-mono text-ink-muted">—</span>;
  return <span className="font-mono text-ink-muted">{rttText(p.rttMs)} ms</span>;
}

// The detail column: quiet, one focal figure. The round trip this side's
// own heartbeats measured, a bar for how many of the recent dials were
// answered, and everything else at hint size. A detail pane, not a second
// dashboard.
export function PeerDetail({ facts, onShare, onSettings, onRemove, onUnpaired }: {
  facts: PeerFacts;
  onShare: () => void;
  onSettings: () => void;
  onRemove: () => void;
  onUnpaired: () => void;
}) {
  const { peer: p, holds, shares } = facts;
  const state = peerState(p);
  const label = peerLabel(p);
  const attempts = p.linkAttempts ?? 0;
  const answered = p.linkAnswered ?? 0;
  const pct = attempts > 0 ? (answered / attempts) * 100 : 0;

  const caption =
    state === "unpaired" ? `Unpaired by them ${p.unpairedAt ? timeAgo(p.unpairedAt) : ""}`.trim()
    : state === "unreachable" ? `No answer${p.dialFailingSince ? ` since ${timeAgo(p.dialFailingSince)}` : ""}`
    : !p.url ? "They dial us · no round trip to measure"
    : state === "off" ? `Quiet${p.lastSeenAt ? ` since ${timeAgo(p.lastSeenAt)}` : ""}`
    : p.rttMs === undefined ? "No round trip measured yet"
    : `Round trip · ${answered} of ${plural(attempts, "heartbeat")} answered`;

  const rows: [string, string][] = [
    ["Version", p.fogVersion ? versionLabel(p.fogVersion) : "unknown"],
    ["Seen", seenText(p)],
    ["Address", p.url ? "we dial them" : "they dial us"],
    ["Paired", timeAgo(p.createdAt)],
  ];

  return (
    <aside className="border-t border-hair px-[26px] pb-10 pt-[30px] lg:border-l lg:border-t-0">
      <div className="flex flex-col gap-[26px]">
        <div>
          <Dot tone={peerTone(p)} title={stateWord(p)} />
          <div className="mt-3.5 text-[19px] font-medium leading-tight text-ink">{label}</div>
          <div className="mt-1.5 truncate font-mono text-hint text-ink-muted" title={p.fingerprintFull}>
            {label !== p.name && <>{p.name} · </>}{p.fingerprint ?? p.instanceId}
          </div>
        </div>

        <div>
          <div className="text-figure-sm font-medium text-ink">{p.rttMs !== undefined && state !== "unreachable" ? <>{rttText(p.rttMs)}<span className="unit">ms</span></> : "—"}</div>
          <div className="mt-2"><Bar pct={pct} tone={state === "ok" ? undefined : "warn"} /></div>
          <div className={`mt-1.5 text-hint ${state === "ok" || state === "off" ? "text-ink-muted" : "text-warn"}`}>{caption}</div>
        </div>

        <div className="flex flex-col gap-[9px]">
          {rows.map(([k, v]) => (
            <div key={k} className="flex justify-between gap-3.5">
              <span className="text-hint text-ink-muted">{k}</span>
              <span className="text-cell text-ink">{v}</span>
            </div>
          ))}
        </div>

        <div className="flex flex-wrap gap-2">
          <Pill tone={holds.length ? "ok" : "off"}>{holds.length ? `Holds ${plural(holds.length, "folder")}` : "Holds nothing"}</Pill>
          <Pill tone={shares.length ? "neutral" : "off"}>{shares.length ? `Shares ${plural(shares.length, "folder")}` : "Shares nothing"}</Pill>
        </div>

        {state === "unpaired" ? (
          <div className="space-y-3">
            <p className="text-hint leading-relaxed text-ink-muted">
              Their fog answers our heartbeats with "no record of this instance". Every setting here
              describes a pairing only this side still has.
            </p>
            <Button size="sm" onClick={onUnpaired}>What happened, and my options</Button>
          </div>
        ) : (
          <div className="flex flex-wrap gap-2">
            <Button size="sm" variant="primary" onClick={onShare}>Share…</Button>
            <Button size="sm" variant="ghost" onClick={onSettings}>Settings…</Button>
            <Button size="sm" variant="ghost" onClick={onRemove}>Remove…</Button>
          </div>
        )}
      </div>
    </aside>
  );
}

