// The Peers tab's map: this fog at the centre, every peer around it, one
// line per pairing, drawn with React Flow so it pans, zooms and refits to
// whatever width the column has.
//
// A card is an instance — who it is, what it runs, how its link is doing.
// What a pairing carries lives on its line: a label quoting the agreement in
// each direction ("→ 2 folders · errors"), which opens into the specifics.
// A dot travels a line only while the peer answers and something is agreed
// to cross, in the direction it moves. Two hues, as everywhere: beacon for a
// link that answers, amber for one that needs you, grey for a quiet one.

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Background,
  BackgroundVariant,
  EdgeLabelRenderer,
  Handle,
  Panel,
  Position,
  ReactFlow,
  ReactFlowProvider,
  getStraightPath,
  useReactFlow,
  type Edge,
  type EdgeProps,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/base.css";
import type { FedCapability, FedIdentity, FedPeer } from "../../api";
import { peerLabel, plural, versionLabel } from "../../lib/format";
import { useTheme } from "../../theme";
import { Dot } from "../app/cockpit";
import { peerState, peerTone, rttText, subState, type PeerState } from "./data";
import type { PeerFacts } from "./Peers";

// Card boxes are fixed so a line can be trimmed to their borders with
// arithmetic instead of measurement.
const SELF = { w: 212, h: 62 };
const PEER = { w: 212, h: 58 };

// Where a label rides its line: the midpoint, which the layout keeps long
// enough that a label clears both cards.
const LABEL_AT = 0.5;

const FIT = { padding: 0.14, maxZoom: 1 } as const;

// Both ends of every line anchor at a card's centre; the edge trims itself
// to the border. The handles are invisible and inert.
const ANCHOR = {
  left: "50%",
  top: "50%",
  opacity: 0,
  pointerEvents: "none",
} as const;

function Anchors() {
  return (
    <>
      <Handle
        type="source"
        position={Position.Top}
        id="c"
        style={ANCHOR}
        isConnectable={false}
      />
      <Handle
        type="target"
        position={Position.Top}
        id="c"
        style={ANCHOR}
        isConnectable={false}
      />
    </>
  );
}

// One thing crossing a pairing. `short` rides the line; `detail` is the
// sentence the label opens into.
type Flow = { short: string; detail: string };
type Flows = { out: Flow[]; in: Flow[] };

// What crosses a pairing, sorted by where the content travels rather than
// who granted what: accepting a peer's errors is inbound even though the
// grant is ours. Counts quote the rows that authorise the flow.
function pairingFlows({ peer, holds, shares }: PeerFacts, name: string): Flows {
  const cap = (capability: string, direction: FedCapability["direction"]) =>
    peer.capabilities.find(
      (c) => c.capability === capability && c.direction === direction,
    );
  const out: Flow[] = [];
  const inbound: Flow[] = [];

  // The counts ride the federation document itself; the folder names come
  // from the share reads, which may land a moment later.
  const held = peer.filesShared ?? holds.length;
  if (held > 0) {
    out.push({
      short: plural(held, "folder"),
      detail: holds.length
        ? `${name} holds copies of ${list(holds.map((h) => h.title))}, each through the lens on its share.`
        : `${name} holds copies of ${plural(held, "folder")} we share.`,
    });
  }
  const received = peer.filesReceived ?? shares.length;
  if (received > 0) {
    const titles = shares.map((s) => {
      const st = subState(s);
      return st.tone === "warn" ? `${s.title} (${st.text})` : s.title;
    });
    inbound.push({
      short: plural(received, "folder"),
      detail: titles.length
        ? `We hold copies of ${list(titles)} from ${name}.`
        : `We hold copies of ${plural(received, "folder")} ${name} shares with us.`,
    });
  }

  const sending = cap("flare.ingest", "use");
  if (sending) {
    const full = sending.config?.mode === "full";
    out.push({
      short: full ? "errors" : "errors, redacted",
      detail: full
        ? `Errors raised here reach ${name} in full, message and stack included.`
        : `Errors raised here reach ${name} redacted: group, count and timing, no messages or payloads.`,
    });
  }
  if (cap("flare.ingest", "grant")) {
    inbound.push({
      short: "errors",
      detail: `${name} may relay its errors here; they land in this fog's error inbox beside our own.`,
    });
  }

  const showing = cap("apps.directory", "grant");
  if (showing) {
    const named = (showing.config?.apps as string[] | undefined) ?? [];
    out.push({
      short: named.length ? plural(named.length, "app") : "all apps",
      detail: named.length
        ? `${name} sees ${list(named)} in its app directory.`
        : `${name} sees every routed app here; the list follows whatever is routed.`,
    });
  }
  if (cap("apps.directory", "use")) {
    const theirs = peer.directory;
    inbound.push({
      short: theirs.length ? plural(theirs.length, "app") : "apps",
      detail: theirs.length
        ? `${name} shows us ${list(theirs.map((a) => a.title || a.app))}.`
        : `Apps ${name} shows us would appear here; its last heartbeat carried none.`,
    });
  }

  if (cap("apps.introduce", "grant")) {
    inbound.push({
      short: "connect",
      detail: `Apps on ${name} may connect to apps here. Each connection is a request our app answers for itself.`,
    });
  }
  return { out, in: inbound };
}

function list(items: string[]): string {
  if (items.length <= 1) return items.join("");
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

// A card's second line: the link as this side measured it, or the trouble.
function linkLine(
  p: FedPeer,
  state: PeerState,
): { text: string; warn: boolean } {
  const seen = p.lastSeenAt ? `seen ${seenWord(p.lastSeenAt)}` : "never seen";
  switch (state) {
    case "unpaired":
      return { text: "unpaired us", warn: true };
    case "unreachable":
      return { text: `no answer · ${seen}`, warn: true };
    case "off":
      return { text: `quiet · ${seen}`, warn: false };
  }
  if (!p.url) return { text: `they dial us · ${seen}`, warn: false };
  if (p.rttMs === undefined) return { text: seen, warn: false };
  return { text: `${rttText(p.rttMs)} ms · ${seen}`, warn: false };
}

function seenWord(at: string): string {
  const s = (Date.now() - new Date(at).getTime()) / 1000;
  if (s < 60) return "now";
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

type SelfData = { identity: FedIdentity };
type PeerData = { peer: FedPeer; selected: boolean; dim: boolean };
type FedNode = Node<SelfData, "self"> | Node<PeerData, "peer">;

type EdgeData = {
  name: string;
  self: string;
  flows: Flows;
  state: PeerState;
  selected: boolean;
  dim: boolean;
  // One open label at a time, held by the map so opening one closes the last.
  open: boolean;
  still: boolean;
  onToggle: () => void;
};

function SelfNode({ data }: NodeProps<Node<SelfData, "self">>) {
  const { identity } = data;
  return (
    <div
      style={{ width: SELF.w, height: SELF.h }}
      title="This fog — its identity, address and reachability"
      className="cursor-pointer rounded-lg bg-fog-950"
    >
      <Anchors />
      <div className="flex h-full flex-col justify-center gap-1 rounded-lg border border-accent/40 bg-accent/[.10] px-3.5 transition-colors duration-150 hover:bg-accent/[.16]">
        <div className="flex items-center gap-[9px]">
          <span className="inline-block h-2.5 w-2.5 shrink-0 rounded-full bg-accent" />
          <span className="truncate text-cell font-medium text-ink">
            {identity.name}
          </span>
          <span className="ml-auto shrink-0 text-xs text-ink-muted">
            this fog
          </span>
        </div>
        <div className="flex items-baseline gap-2 text-hint text-ink-muted">
          <span className="truncate font-mono">
            {identity.fingerprint ?? identity.instanceId}
          </span>
          {identity.fogVersion && (
            <span className="ml-auto shrink-0 font-mono text-xs">
              {versionLabel(identity.fogVersion)}
            </span>
          )}
        </div>
      </div>
    </div>
  );
}

function PeerNode({ data }: NodeProps<Node<PeerData, "peer">>) {
  const { peer: p, selected, dim } = data;
  const state = peerState(p);
  const link = linkLine(p, state);
  const trouble = state === "unreachable" || state === "unpaired";
  return (
    <div
      style={{ width: PEER.w, height: PEER.h, opacity: dim ? 0.7 : 1 }}
      className="cursor-pointer rounded-lg bg-fog-950 transition-opacity duration-150"
    >
      <Anchors />
      <div
        className={`flex h-full flex-col justify-center gap-1 rounded-lg border px-3.5 transition-colors duration-150 ${
          selected
            ? "border-accent/45 bg-accent/[.12]"
            : trouble
              ? "border-warn/30 bg-warn/[.06] hover:bg-warn/10"
              : "border-veil-edge bg-veil hover:bg-hover"
        }`}
      >
        <div className="flex items-center gap-[9px]">
          <Dot tone={peerTone(p)} />
          <span
            className={`truncate text-cell ${state === "ok" ? "text-ink" : "text-ink-muted"}`}
          >
            {peerLabel(p)}
          </span>
          {p.fogVersion && (
            <span className="ml-auto shrink-0 font-mono text-xs text-ink-muted">
              {versionLabel(p.fogVersion)}
            </span>
          )}
        </div>
        <div
          className={`truncate text-hint ${link.warn ? "text-warn" : "text-ink-muted"}`}
        >
          {link.text}
        </div>
      </div>
    </div>
  );
}

// borderPoint: walk from a card's centre toward the other end and stop where
// the line leaves the box, plus a gap, so no stroke runs under a card.
function borderPoint(
  cx: number,
  cy: number,
  dx: number,
  dy: number,
  halfW: number,
  halfH: number,
  gap: number,
) {
  const len = Math.hypot(dx, dy) || 1;
  const ux = dx / len;
  const uy = dy / len;
  const tx = ux === 0 ? Infinity : Math.abs(halfW / ux);
  const ty = uy === 0 ? Infinity : Math.abs(halfH / uy);
  const t = Math.min(tx, ty) + gap;
  return [cx + ux * t, cy + uy * t] as const;
}

function PairingEdge({
  sourceX,
  sourceY,
  targetX,
  targetY,
  data,
}: EdgeProps<Edge<EdgeData>>) {
  const d = data as EdgeData;
  const dx = targetX - sourceX;
  const dy = targetY - sourceY;
  const [x1, y1] = borderPoint(
    sourceX,
    sourceY,
    dx,
    dy,
    SELF.w / 2,
    SELF.h / 2,
    5,
  );
  const [x2, y2] = borderPoint(
    targetX,
    targetY,
    -dx,
    -dy,
    PEER.w / 2,
    PEER.h / 2,
    5,
  );
  const [path] = getStraightPath({
    sourceX: x1,
    sourceY: y1,
    targetX: x2,
    targetY: y2,
  });
  const labelX = x1 + (x2 - x1) * LABEL_AT;
  const labelY = y1 + (y2 - y1) * LABEL_AT;

  const live = d.state === "ok";
  const trouble = d.state === "unreachable" || d.state === "unpaired";
  const stroke = trouble
    ? "var(--color-warn)"
    : live
      ? "var(--color-accent)"
      : "var(--color-fog-600)";
  const opacity = d.dim ? 0.3 : d.selected ? 0.95 : 0.5;

  // A dot per direction that carries anything, only while the peer answers.
  const dots =
    live && !d.still
      ? [
          ...(d.flows.out.length ? [{ reverse: false }] : []),
          ...(d.flows.in.length ? [{ reverse: true }] : []),
        ]
      : [];
  // Nothing crosses a pairing the other side ended, whatever our rows say.
  const labelled =
    d.state !== "unpaired" && (d.flows.out.length > 0 || d.flows.in.length > 0);

  return (
    <>
      <g opacity={opacity} style={{ transition: "opacity 150ms" }}>
        <path
          d={path}
          fill="none"
          stroke={stroke}
          strokeWidth={d.selected ? 2 : 1.25}
          strokeDasharray={live ? undefined : "4 5"}
        />
        {dots.map((dot, i) => (
          <circle key={String(dot.reverse)} r={2.75} fill="var(--color-accent)">
            <animateMotion
              key={path}
              dur="3.2s"
              begin={`-${i * 1.6}s`}
              repeatCount="indefinite"
              path={path}
              calcMode="linear"
              keyPoints={dot.reverse ? "1;0" : "0;1"}
              keyTimes="0;1"
            />
          </circle>
        ))}
      </g>
      {labelled && (
        <EdgeLabelRenderer>
          <div
            className="nodrag nopan absolute"
            style={{
              transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`,
              opacity: d.dim && !d.open ? 0.6 : 1,
              zIndex: d.open ? 10 : undefined,
              pointerEvents: "all",
              transition: "opacity 150ms",
            }}
          >
            <button
              type="button"
              aria-expanded={d.open}
              aria-label={`What crosses between this fog and ${d.name}`}
              onClick={(e) => {
                // The pane behind clears the selection on click.
                e.stopPropagation();
                d.onToggle();
              }}
              className={`block rounded-md border bg-fog-950 px-2 py-1 text-left text-[11px] leading-[1.4] transition-colors duration-150 ${
                d.open
                  ? "border-edge-strong"
                  : "border-veil-edge hover:border-edge-hover"
              }`}
            >
              {d.flows.out.length > 0 && (
                <FlowLine arrow="→" flows={d.flows.out} live={live} />
              )}
              {d.flows.in.length > 0 && (
                <FlowLine arrow="←" flows={d.flows.in} live={live} />
              )}
            </button>
            {/* The centre card sits at the origin: a label below it opens
                upward rather than off the bottom of the frame. */}
            {d.open && <FlowDetails d={d} above={labelY > 0} />}
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}

function FlowLine({
  arrow,
  flows,
  live,
}: {
  arrow: string;
  flows: Flow[];
  live: boolean;
}) {
  return (
    <div className="flex items-center gap-1.5 whitespace-nowrap">
      <span className={live ? "text-accent" : "text-ink-faint"}>{arrow}</span>
      <span className={live ? "text-ink" : "text-ink-muted"}>
        {flows.map((f) => f.short).join(" · ")}
      </span>
    </div>
  );
}

function FlowDetails({ d, above }: { d: EdgeData; above: boolean }) {
  return (
    <div
      // Clicks inside are for reading, not for dismissing.
      onClick={(e) => e.stopPropagation()}
      className={`absolute left-1/2 w-72 -translate-x-1/2 ${above ? "bottom-full pb-1.5" : "top-full pt-1.5"}`}
    >
      <div className="max-h-64 space-y-3 overflow-y-auto rounded-lg border border-edge bg-fog-900 p-3 text-xs leading-snug shadow-xl shadow-black/30">
        {d.state === "unreachable" && (
          <p className="text-warn">
            Our dials to {d.name} aren't answered. These terms stand; whatever
            needs to reach it waits.
          </p>
        )}
        {d.state === "off" && (
          <p className="text-ink-muted">
            {d.name} is quiet. These terms apply when it's back.
          </p>
        )}
        <FlowSection title={`From ${d.self}`} flows={d.flows.out} />
        <FlowSection title={`To ${d.self}`} flows={d.flows.in} />
      </div>
    </div>
  );
}

function FlowSection({ title, flows }: { title: string; flows: Flow[] }) {
  if (flows.length === 0) return null;
  return (
    <div>
      <div className="subhead mb-1.5">{title}</div>
      <ul className="space-y-1.5">
        {flows.map((f) => (
          <li key={f.short} className="text-ink-muted">
            <span className="text-ink">{f.short}</span> — {f.detail}
          </li>
        ))}
      </ul>
    </div>
  );
}

const nodeTypes = { self: SelfNode, peer: PeerNode };
const edgeTypes = { pairing: PairingEdge };

interface MapProps {
  identity: FedIdentity;
  facts: PeerFacts[];
  selected: number | null;
  onSelect: (id: number | null) => void;
  onSelf: () => void;
}

function Map({ identity, facts, selected, onSelect, onSelf }: MapProps) {
  const { fitView, zoomIn, zoomOut } = useReactFlow();
  const { theme } = useTheme();
  const [openLabel, setOpenLabel] = useState<number | null>(null);
  const still = useMemo(
    () =>
      window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false,
    [],
  );
  const n = facts.length;

  // A label belongs to the selected pairing: choosing another peer anywhere
  // (the table, the sidebar, Esc) closes it.
  useEffect(() => {
    setOpenLabel((cur) => (cur === selected ? cur : null));
  }, [selected]);

  // Peers on an ellipse around the centre, wider than tall because the cards
  // are, growing with the fleet so cards never crowd. One or two peers sit
  // beside the centre; more start at the top.
  const nodes = useMemo<FedNode[]>(() => {
    const rx = Math.max(
      SELF.w / 2 + PEER.w / 2 + 240,
      ((PEER.w + 90) * n) / (2 * Math.PI),
    );
    const ry = Math.max(
      SELF.h / 2 + PEER.h / 2 + 120,
      ((PEER.h + 110) * n) / (2 * Math.PI),
    );
    const start = n <= 2 ? 0 : -Math.PI / 2;
    return [
      {
        id: "self",
        type: "self" as const,
        position: { x: -SELF.w / 2, y: -SELF.h / 2 },
        data: { identity },
        draggable: false,
      },
      ...facts.map(({ peer: p }, i) => {
        const a = start + (2 * Math.PI * i) / Math.max(1, n);
        return {
          id: `peer-${p.id}`,
          type: "peer" as const,
          position: {
            x: Math.cos(a) * rx - PEER.w / 2,
            y: Math.sin(a) * ry - PEER.h / 2,
          },
          data: {
            peer: p,
            selected: selected === p.id,
            dim: selected !== null && selected !== p.id,
          },
          draggable: false,
        };
      }),
    ];
  }, [identity, facts, n, selected]);

  const edges = useMemo<Edge<EdgeData>[]>(
    () =>
      facts.map((f) => {
        const p = f.peer;
        const name = peerLabel(p);
        return {
          id: `pair-${p.id}`,
          source: "self",
          target: `peer-${p.id}`,
          sourceHandle: "c",
          targetHandle: "c",
          type: "pairing",
          data: {
            name,
            self: identity.name,
            flows: pairingFlows(f, name),
            state: peerState(p),
            selected: selected === p.id,
            dim: selected !== null && selected !== p.id,
            open: openLabel === p.id,
            still,
            onToggle: () => {
              setOpenLabel((cur) => (cur === p.id ? null : p.id));
              if (selected !== p.id) onSelect(p.id);
            },
          },
        };
      }),
    [facts, identity.name, selected, openLabel, still, onSelect],
  );

  // Refit when the fleet changes shape or the canvas does (the detail column
  // opening, a resize) — not on every poll, which would fight a pan. The
  // canvas is watched directly: selecting a peer from a line label opens the
  // detail column in the same commit, and the store's pane size missed that.
  const frame = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = frame.current;
    if (!el) return;
    let raf = 0;
    const refit = () => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => fitView({ ...FIT, duration: 200 }));
    };
    const ro = new ResizeObserver(refit);
    ro.observe(el);
    return () => {
      ro.disconnect();
      cancelAnimationFrame(raf);
    };
  }, [n, fitView]);

  return (
    <div ref={frame} className="h-full w-full">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        colorMode={theme}
        fitView
        fitViewOptions={FIT}
        minZoom={0.3}
        maxZoom={1.5}
        nodesDraggable={false}
        nodesConnectable={false}
        nodesFocusable={false}
        edgesFocusable={false}
        elementsSelectable={false}
        zoomOnDoubleClick={false}
        zoomOnScroll={false}
        preventScrolling={false}
        onNodeClick={(_, node) => {
          setOpenLabel(null);
          if (node.type === "self") {
            onSelf();
            return;
          }
          const id = (node.data as PeerData).peer.id;
          onSelect(selected === id ? null : id);
        }}
        onPaneClick={() => {
          setOpenLabel(null);
          onSelect(null);
        }}
      >
        <Background
          variant={BackgroundVariant.Dots}
          gap={18}
          size={1}
          color="var(--color-edge)"
        />
        <Panel position="top-right">
          <div className="flex overflow-hidden rounded-md border border-veil-edge bg-fog-950 text-cell text-ink-muted">
            {(
              [
                ["+", "Zoom in", () => zoomIn({ duration: 150 })],
                ["−", "Zoom out", () => zoomOut({ duration: 150 })],
                [
                  "Fit",
                  "Fit the map to the frame",
                  () => fitView({ ...FIT, duration: 200 }),
                ],
              ] as const
            ).map(([glyph, label, run]) => (
              <button
                key={label}
                type="button"
                title={label}
                aria-label={label}
                onClick={run}
                className="h-7 min-w-7 border-l border-veil-edge px-2 transition-colors duration-150 first:border-l-0 hover:bg-hover hover:text-ink"
              >
                {glyph}
              </button>
            ))}
          </div>
        </Panel>
      </ReactFlow>
    </div>
  );
}

function LegendLine({
  stroke,
  dashed,
  label,
}: {
  stroke: string;
  dashed?: boolean;
  label: string;
}) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <svg width="18" height="2" aria-hidden>
        <line
          x1="0"
          y1="1"
          x2="18"
          y2="1"
          stroke={stroke}
          strokeWidth="1.5"
          strokeDasharray={dashed ? "4 3" : undefined}
        />
      </svg>
      {label}
    </span>
  );
}

export default function PeerMap(props: MapProps) {
  // One fixed frame whatever the fleet does: pairing or unpairing a peer
  // redraws inside it and never moves the page under the cursor.
  // The legend sits under the frame, where no card can land on it.
  return (
    <div>
      <div className="h-[400px] overflow-hidden rounded-lg border border-veil-edge bg-veil [&_.react-flow\_\_attribution]:bg-transparent [&_.react-flow\_\_attribution_a]:text-ink-faint">
        <ReactFlowProvider>
          <Map {...props} />
        </ReactFlowProvider>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-ink-muted">
        <span>
          <span className="text-accent">→</span> from this fog
        </span>
        <span>
          <span className="text-accent">←</span> to this fog
        </span>
        <LegendLine stroke="var(--color-accent)" label="answering" />
        <LegendLine stroke="var(--color-fog-600)" dashed label="quiet" />
        <LegendLine stroke="var(--color-warn)" dashed label="needs you" />
        <span className="text-ink-faint">Click a line for what crosses it</span>
      </div>
    </div>
  );
}
