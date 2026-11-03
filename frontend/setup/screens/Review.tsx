import { useState } from "react";
import { formatDiskSize } from "../../src/lib/format";
import Button from "../../src/components/Button";
import { Spinner, errorText } from "../../src/components/Async";
import type { Inventory, KitInfo } from "../types";

export function ReviewScreen({
  inv,
  mode,
  kit,
  disk,
  serverName,
  username,
  onBack,
  onInstall,
}: {
  inv: Inventory | null;
  mode: "fresh" | "restore";
  kit: KitInfo | null;
  disk: string;
  serverName: string;
  username: string;
  onBack: () => void;
  onInstall: () => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const target = inv?.disks.find((d) => d.path === disk);

  const rows: [string, string][] = [
    ["Install to", target ? `${target.model || target.path} (${formatDiskSize(target.sizeBytes)})` : disk],
    ["Server name", `${serverName} — http://${serverName}.local`],
    mode === "restore"
      ? ["Restores", `server "${kit?.serverName || serverName}"${kit?.adminUser ? ` (admin: ${kit.adminUser})` : ""}`]
      : ["Your account", username],
    ["Erases", target ? target.contents : "everything on the disk"],
  ];

  return (
    <div className="space-y-5">
      <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">
        {mode === "restore" ? "Ready to restore" : "Ready to install"}
      </h2>
      <dl className="divide-y divide-divider rounded-lg border border-hair">
        {rows.map(([k, v]) => (
          <div key={k} className="flex justify-between gap-4 px-4 py-3">
            <dt className="text-sm text-ink-muted">{k}</dt>
            <dd className="text-right text-sm text-ink">{v}</dd>
          </div>
        ))}
      </dl>
      <p className="text-xs text-ink-muted">
        Installing takes a few minutes. The disk is erased as the first step.
        {mode === "restore" &&
          " Your backup kit is already verified and held in memory — a wrong passphrase or corrupt kit can no longer surface after the erase."}
      </p>
      {error && <p className="text-sm text-warn">{error}</p>}
      <div className="flex gap-3">
        <Button className="flex-1" onClick={onBack} disabled={busy}>
          Back
        </Button>
        <Button
          variant="primary"
          className="flex-1"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            setError("");
            try {
              await onInstall();
            } catch (err) {
              setError(errorText(err, "The install could not start."));
              setBusy(false);
            }
          }}
        >
          {busy ? <Spinner /> : mode === "restore" ? "Erase disk and restore Fog" : "Erase disk and install Fog"}
        </Button>
      </div>
    </div>
  );
}
