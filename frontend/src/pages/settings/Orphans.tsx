// Settings › Storage › Orphaned data: leftovers from deleted apps or
// edited specs.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, type Orphan } from "../../api";
import { LoadError, Loading, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import { RowItem, RowList } from "../../components/SettingRow";
import { formatBytes } from "../../lib/format";
import { SettingsPage } from "./Frame";

const ORPHAN_LABELS: Record<string, string> = {
  data: "app data (app deleted)",
  volume: "volume removed from spec",
  backups: "backups (app deleted)",
  builds: "build checkout",
};

// Leftovers from deleted apps or edited specs: kept deliberately (data is
// never removed without an explicit ask), surfaced here so they can be
// reclaimed instead of silently eating the disk.
export default function Orphans() {
  const [ask, dialog] = useConfirm();
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const orphans = useQuery({
    queryKey: ["orphans"],
    queryFn: () => get<{ orphans: Orphan[] }>("/api/system/orphans"),
  });

  const reclaim = useMutation({
    mutationFn: (path: string) => post("/api/system/orphans/reclaim", { path }),
    onSuccess: () => {
      setError("");
      // Awaited: the row disappears as part of the action completing, not a
      // moment later, so nothing lingers that has already been deleted.
      return qc.invalidateQueries({ queryKey: ["orphans"] });
    },
    onError: (e) => setError(errorText(e, "Reclaim failed")),
  });

  const list = orphans.data?.orphans ?? [];
  const phase = phaseOf(orphans, orphans.data?.orphans.length);
  return (
    <SettingsPage description="Data on disk that no installed app owns — kept, because data is never removed without an explicit ask, and listed here so it can be reclaimed instead of silently eating the disk.">
      {phase === "loading" ? (
        <Loading label="Scanning the data directory…" />
      ) : phase === "error" ? (
        // A failed scan is not a clean disk — never report it as one.
        <LoadError label="Couldn't scan for orphaned data." error={orphans.error} onRetry={() => orphans.refetch()} />
      ) : phase === "empty" ? (
        <p className="text-xs text-ink-muted">No orphaned data — everything on disk belongs to an installed app.</p>
      ) : (
        <RowList>
          {list.map((o) => (
            <RowItem
              key={o.path}
              title={<>{o.app} <span className="font-normal text-ink-muted">— {ORPHAN_LABELS[o.kind] ?? o.kind}</span></>}
              meta={<span className="block truncate font-mono">{o.path}</span>}
              actions={
                <>
                  <span className="text-xs tabular-nums text-ink-muted">{formatBytes(o.sizeBytes)}</span>
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={reclaim.isPending}
                    onClick={async () => {
                      if (await ask({ title: `Delete ${o.path}?`, body: `${formatBytes(o.sizeBytes)} of ${o.app}'s leftovers. This cannot be undone.`, action: "Delete", danger: true })) {
                        reclaim.mutate(o.path);
                      }
                    }}
                  >
                    {reclaim.isPending && reclaim.variables === o.path ? "Deleting…" : "Reclaim"}
                  </Button>
                </>
              }
            />
          ))}
        </RowList>
      )}
      {error && <p className="text-sm text-warn">{error}</p>}
      {dialog}
    </SettingsPage>
  );
}
