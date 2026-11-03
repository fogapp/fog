// The Activity tab: the ledger of what crossed a federation boundary,
// newest first, in fogd's own vocabulary. The preference about being told
// lives on the Config tab.

import type { FederationLedgerEntry } from "../../api";
import { LoadError, Loading } from "../../components/Async";
import { Group, whenShort } from "../app/cockpit";

// A crossing that ended something reads amber; trust and sharing read
// beacon; discovery is information.
export function ledgerTone(e: FederationLedgerEntry): "warn" | "ok" | "info" {
  if (/unpair|revoke|reject|delete|withdraw|unsubscribe/.test(e.action)) return "warn";
  return e.category === "discovery" ? "info" : "ok";
}

const toneClass = { warn: "text-warn", ok: "text-accent", info: "text-ink-muted" } as const;
const toneWord = { warn: "warn", ok: "ok", info: "info" } as const;

export function ActivityTab({ entries, loading, error, onRetry }: {
  entries: FederationLedgerEntry[];
  loading: boolean;
  error: unknown;
  onRetry: () => void;
}) {
  return (
    <div>
      <Group title="Connect activity" first right={<span className="text-hint text-ink-muted">Newest first</span>}>
        {loading ? (
          <Loading label="Loading the ledger…" />
        ) : error && entries.length === 0 ? (
          <LoadError label="Couldn't load the ledger." error={error} onRetry={onRetry} />
        ) : entries.length === 0 ? (
          <p className="text-hint text-ink-muted">Nothing has crossed a household boundary yet.</p>
        ) : (
          <div className="flex flex-col">
            {entries.map((e) => {
              const tone = ledgerTone(e);
              return (
                <div key={e.id} className="grid items-center gap-3.5 border-b border-divider py-[9px]" style={{ gridTemplateColumns: "118px 44px minmax(0,1fr)" }}>
                  <span className="font-mono text-hint text-ink-muted">{whenShort(e.at)}</span>
                  <span className={`text-xs ${toneClass[tone]}`}>{toneWord[tone]}</span>
                  <span className="min-w-0 truncate">
                    {e.source && <span className="mr-1.5 rounded bg-fog-800 px-1 py-0.5 text-xs font-medium text-ink-muted">{e.source}</span>}
                    <span className="font-mono text-cell text-ink">{e.action}</span>
                    <span className="text-cell text-ink-muted"> · {e.summary}</span>
                  </span>
                </div>
              );
            })}
          </div>
        )}
      </Group>
    </div>
  );
}
