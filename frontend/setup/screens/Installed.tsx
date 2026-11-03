import { useState } from "react";
import { post } from "../../src/api";
import Button from "../../src/components/Button";
import type { ExistingInstall } from "../types";

// The screen for the most likely way to meet this installer twice: the USB
// stick was left plugged in, so the machine booted it again instead of the
// server it just became. Nothing here touches the disk — the fix is
// physical, and the only job is saying so plainly.
export function InstalledScreen({
  existing,
  onInstallAnyway,
}: {
  existing: ExistingInstall | null;
  onInstallAnyway: () => void;
}) {
  const [rebooting, setRebooting] = useState(false);
  const when = fmtInstalledAt(existing?.installedAt);
  const url = existing?.serverName ? `http://${existing.serverName}.local:8080` : "";

  if (rebooting) {
    return (
      <div className="space-y-4 text-center">
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">Restarting</h2>
        <p className="text-sm text-ink-muted">
          <strong className="text-ink">Take the USB stick out now</strong>, while the
          screen is dark. The machine then starts your server instead of this installer.
        </p>
        {url && (
          <p className="text-xs text-ink-muted">
            Your dashboard is at <span className="font-mono text-ink-muted">{url}</span>
          </p>
        )}
      </div>
    );
  }

  return (
    <div className="space-y-5">
      <div className="text-center">
        <span className="text-4xl">✓</span>
        <h2 className="mt-3 text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">
          This machine is already a Fog server
        </h2>
        <p className="mt-2 text-sm text-ink-muted">
          Fog is installed on {existing?.disk || "this machine's disk"}
          {when && <> — set up {when}</>}. It started the installer instead because the USB
          stick is still plugged in. Nothing has been changed.
        </p>
      </div>
      <div className="rounded-lg border border-veil-edge bg-veil p-4">
        <p className="text-sm font-medium text-ink">To start your server</p>
        <ol className="mt-2 list-decimal space-y-1 pl-5 text-sm text-ink-muted">
          <li>Restart the machine — the button below, or its power button.</li>
          <li>Pull the USB stick out while the screen is dark.</li>
          <li>
            It comes back as your server
            {url && (
              <>
                , at <span className="font-mono text-xs text-ink">{url}</span>
              </>
            )}
            .
          </li>
        </ol>
        <p className="mt-2 text-xs text-ink-muted">
          The stick is worth keeping: it is also this machine&apos;s recovery tool.
        </p>
      </div>
      <Button
        variant="primary"
        className="w-full py-2.5"
        onClick={async () => {
          setRebooting(true);
          try {
            await post("/api/setup/reboot");
          } catch {
            /* the agent may die mid-response as the machine goes down */
          }
        }}
      >
        Restart
      </Button>
      <p className="text-center text-xs text-ink-muted">
        Replacing this server instead?{" "}
        <button className="underline hover:text-ink-muted" onClick={onInstallAnyway}>
          Install Fog again
        </button>{" "}
        — that erases everything on the disk, including the apps and data already here.
      </p>
    </div>
  );
}

// fmtInstalledAt renders the marker's timestamp for humans, and nothing at
// all when it can't be read as a date — a wrong date is worse than none.
function fmtInstalledAt(raw?: string): string {
  if (!raw) return "";
  const d = new Date(raw);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}
