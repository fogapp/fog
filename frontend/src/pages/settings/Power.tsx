// Settings › Server › Power: reboot or shut down the host from the UI —
// the box usually has no keyboard attached, so without this "restart the
// server" means pulling the plug.

import { useEffect, useRef, useState } from "react";
import { get, post, type SetupStatus } from "../../api";
import { Spinner, errorText } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import { usePollUntilRestart } from "../../lib/usePollUntilRestart";
import { SettingsPage } from "./Frame";

// Reboot or shut down the host from the UI — the box usually has no keyboard
// attached, so without this "restart the server" means pulling the plug. Both
// overlays stay truthful: each appears only after the server acknowledged the
// request, the reboot reloads only once the box has gone down and come back,
// and the shutdown says "off" only once the box has stopped answering.
export default function Power() {
  const [error, setError] = useState("");
  // The shared poll carries the guard this section pioneered: only a
  // recovery after an observed outage counts — polling too early would see
  // the still-running server and reload into the shutdown.
  const restart = usePollUntilRestart();
  const halt = useHaltWatch();
  const [ask, dialog] = useConfirm();

  async function reboot() {
    if (!(await ask({ title: "Reboot the server?", body: "Every app goes down until it's back — usually a minute or two.", action: "Reboot", danger: true }))) return;
    setError("");
    try {
      await post("/api/system/reboot");
      restart.begin({ intervalMs: 3000, deadlineMs: 5 * 60_000 });
    } catch (e) {
      setError(errorText(e, "Reboot request failed"));
    }
  }

  async function shutdown() {
    if (!(await ask({ title: "Shut the server down?", body: "Every app goes down with it, and nothing comes back until someone presses the power button on the box.", action: "Shut down", danger: true }))) return;
    setError("");
    try {
      await post("/api/system/shutdown");
      halt.begin();
    } catch (e) {
      setError(errorText(e, "Shutdown request failed"));
    }
  }

  return (
    <SettingsPage description="After a reboot fog and all apps start again on their own and NAS shares are remounted; after a shutdown nothing comes back until the box is switched on by hand.">
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="danger" onClick={reboot}>Reboot server</Button>
        <Button variant="danger" onClick={shutdown}>Shut down server</Button>
      </div>
      {error && <p className="text-sm text-warn">{error}</p>}
      {dialog}
      {restart.timedOut && (
        <p className="text-sm text-warn">
          The server hasn't come back after five minutes. It may still be booting — reload this
          page manually once it's reachable.
        </p>
      )}
      {restart.waiting && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/80">
          <div className="space-y-2 text-center">
            <div className="flex justify-center"><Spinner size="h-6 w-6" /></div>
            <p className="text-sm font-medium text-ink">Rebooting the server…</p>
            <p className="text-xs text-ink-muted">This page reloads itself when the box is back. Usually a minute or two.</p>
          </div>
        </div>
      )}
      {halt.state !== "idle" && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/80">
          <div className="max-w-sm space-y-2 px-6 text-center">
            {halt.state === "going" ? (
              <>
                <div className="flex justify-center"><Spinner size="h-6 w-6" /></div>
                <p className="text-sm font-medium text-ink">Shutting the server down…</p>
                <p className="text-xs text-ink-muted">This page says so once the box has stopped answering.</p>
              </>
            ) : halt.state === "off" ? (
              <>
                <p className="text-sm font-medium text-ink">The server is off.</p>
                <p className="text-xs text-ink-muted">
                  Press the power button on the box to bring it back — nothing here works until then.
                </p>
              </>
            ) : (
              <>
                <p className="text-sm font-medium text-warn">The server is still answering.</p>
                <p className="text-xs text-ink-muted">
                  It accepted the shutdown but hasn't gone down after two minutes. Check the box itself.
                </p>
              </>
            )}
            {halt.state !== "going" && (
              <Button variant="ghost" size="sm" onClick={halt.dismiss}>Dismiss</Button>
            )}
          </div>
        </div>
      )}
    </SettingsPage>
  );
}

// The mirror of usePollUntilRestart: a shutdown has no coming-back to wait
// for, so the poll watches for the server to STOP answering and reports
// "off" only then. Until the box actually goes quiet the overlay claims
// nothing more than "shutting down".
type HaltState = "idle" | "going" | "off" | "stuck";

function useHaltWatch() {
  const [state, setState] = useState<HaltState>("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  // Bumped on unmount and on every begin(), so a stale tick does nothing.
  const gen = useRef(0);

  useEffect(() => {
    return () => {
      // gen is a counter, not a DOM node: reading its latest value at
      // unmount is the point.
      // eslint-disable-next-line react-hooks/exhaustive-deps
      gen.current++;
      if (timer.current !== undefined) clearTimeout(timer.current);
    };
  }, []);

  function begin() {
    const my = ++gen.current;
    if (timer.current !== undefined) clearTimeout(timer.current);
    setState("going");
    const deadline = Date.now() + 120_000;
    const tick = async () => {
      try {
        await get<SetupStatus>("/api/setup/status");
      } catch {
        // Unreachable: the box has gone down, which is the whole point.
        if (my === gen.current) setState("off");
        return;
      }
      if (my !== gen.current) return;
      if (Date.now() > deadline) {
        setState("stuck");
        return;
      }
      timer.current = setTimeout(() => void tick(), 3000);
    };
    timer.current = setTimeout(() => void tick(), 3000);
  }

  function dismiss() {
    gen.current++;
    if (timer.current !== undefined) clearTimeout(timer.current);
    setState("idle");
  }

  return { state, begin, dismiss };
}
