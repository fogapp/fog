import { useEffect, useState } from "react";
import { formatDiskSize } from "../../src/lib/format";
import Button from "../../src/components/Button";
import { Loading, Spinner } from "../../src/components/Async";
import type { Disk, Inventory } from "../types";

function DiskRow({
  d,
  selected,
  onSelect,
}: {
  d: Disk;
  selected: boolean;
  onSelect: () => void;
}) {
  const disabled = d.bootMedium;
  return (
    <button
      onClick={onSelect}
      disabled={disabled}
      className={`w-full rounded-lg border p-4 text-left transition-colors ${
        selected
          ? "border-fog-400 bg-fog-800"
          : disabled
            ? "cursor-not-allowed border-hair opacity-50"
            : "border-edge bg-veil hover:bg-fog-800"
      }`}
    >
      <div className="flex items-baseline justify-between gap-3">
        <span className="text-sm font-medium text-ink">{d.model || d.path}</span>
        <span className="shrink-0 text-sm text-ink-muted">{formatDiskSize(d.sizeBytes)}</span>
      </div>
      <p className="mt-1 text-xs text-ink-muted">
        {d.path} · {d.bus}
        {d.bootMedium ? " · this is the installer itself" : ` · contains ${d.contents}`}
      </p>
    </button>
  );
}

export function DiskScreen({
  inv,
  invError,
  refresh,
  selected,
  onSelect,
  onBack,
  onDone,
}: {
  inv: Inventory | null;
  invError: string;
  refresh: () => Promise<Inventory | null>;
  selected: string;
  onSelect: (path: string) => void;
  onBack: () => void;
  onDone: () => void;
}) {
  const [showAll, setShowAll] = useState(false);
  const [confirmed, setConfirmed] = useState(false);

  // Auto-pick the unambiguous disk once the inventory lands.
  useEffect(() => {
    if (inv && !selected && inv.candidate) onSelect(inv.candidate);
  }, [inv, selected, onSelect]);

  // Keep looking until a cable shows up — the operator's fix (plugging one
  // in) should be noticed without any button pressing.
  const online = inv?.network.online ?? false;
  useEffect(() => {
    if (online) return;
    const t = setInterval(refresh, 3000);
    return () => clearInterval(t);
  }, [online, refresh]);

  // No inventory yet: the way back must still exist — a machine whose disks
  // can't be read is exactly where someone wants to change their mind.
  if (!inv) {
    return (
      <div className="space-y-4">
        {invError ? (
          <p className="text-sm text-warn">{invError}</p>
        ) : (
          <Loading label="Looking at this machine's disks…" />
        )}
        <div className="flex gap-3">
          <Button className="flex-1" onClick={onBack}>
            Back
          </Button>
          {invError && (
            <Button variant="primary" className="flex-1" onClick={refresh}>
              Try again
            </Button>
          )}
        </div>
      </div>
    );
  }

  const target = inv.disks.find((d) => d.path === selected);
  const ambiguous = !inv.candidate;
  const list = showAll || ambiguous ? inv.disks : inv.disks.filter((d) => d.path === selected);

  return (
    <div className="space-y-5">
      <div>
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">Install destination</h2>
        <p className="mt-1 text-sm text-ink-muted">
          {ambiguous
            ? "This machine has several disks — choose where Fog should live."
            : "Fog found this machine's disk."}
        </p>
      </div>
      {online ? (
        inv.network.ip && (
          <p className="text-xs text-ink-muted">
            Network: connected — {inv.network.ip} via {inv.network.interface}
          </p>
        )
      ) : (
        <div className="space-y-1 rounded-lg border border-veil-edge bg-veil p-4">
          <p className="flex items-center gap-2 text-sm text-ink">
            <Spinner size="h-3.5 w-3.5" /> No network yet
          </p>
          <p className="text-xs text-ink-muted">
            Plug an Ethernet cable into this machine — it will be noticed automatically. A
            home server works best wired, and it's only reachable over the network.
          </p>
        </div>
      )}
      <div className="space-y-2">
        {list.map((d) => (
          <DiskRow
            key={d.path}
            d={d}
            selected={d.path === selected}
            onSelect={() => {
              onSelect(d.path);
              setConfirmed(false);
            }}
          />
        ))}
      </div>
      {!showAll && !ambiguous && inv.disks.length > 1 && (
        <button
          className="text-xs font-medium text-ink-muted underline hover:text-ink"
          onClick={() => setShowAll(true)}
        >
          Show all {inv.disks.length} disks
        </button>
      )}
      {target && (
        <label className="flex cursor-pointer items-start gap-3 rounded-lg border border-warn/30 bg-warn/10 p-4">
          <input
            type="checkbox"
            checked={confirmed}
            onChange={(e) => setConfirmed(e.target.checked)}
            className="mt-0.5 h-4 w-4 accent-warn"
          />
          <span className="text-sm text-warn">
            Erase <strong>{target.model || target.path}</strong> completely — it currently
            contains {target.contents}. This cannot be undone.
          </span>
        </label>
      )}
      <div className="flex gap-3">
        <Button className="flex-1" onClick={onBack}>
          Back
        </Button>
        <Button
          variant="primary"
          className="flex-1"
          disabled={!target || !confirmed || !online}
          onClick={onDone}
        >
          Continue
        </Button>
      </div>
    </div>
  );
}
