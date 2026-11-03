import { useCallback, useEffect, useRef, useState } from "react";
import { get, type SetupStatus } from "../api";

// One shared "wait for the server to come back, then reload" poll. Four
// surfaces used to hand-roll this (self-update in the bell and the Updates
// tab, reboot, restore-kit), each with a different bug: detached timers that
// outlived the component, reloads on the FIRST successful poll — which can
// land before the old process has even exited and reload into it — and hard
// deadlines that reloaded whether or not the restart ever happened.
//
// The rules here:
// - the timer dies with the component;
// - a plain poll only counts once the server has been observed DOWN first
//   (down-then-up), unless `isUp` proves the new process directly;
// - past the deadline the poll stops and reports "timeout" so the caller can
//   say so on screen — it never reloads into a state it can't vouch for.

export type RestartPollState = "idle" | "waiting" | "timeout";

export interface RestartPollOptions {
  // Confirms the process that answered is the NEW one (e.g. its version
  // changed). Without it the guard is down-then-up: a successful poll counts
  // only after at least one failed one.
  isUp?: (status: SetupStatus) => boolean;
  intervalMs?: number;
  initialDelayMs?: number;
  deadlineMs?: number;
}

export function usePollUntilRestart() {
  const [state, setState] = useState<RestartPollState>("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  // Bumped on unmount and on every begin(): a stale tick that awakes after
  // either sees the mismatch and does nothing.
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

  const begin = useCallback((opts: RestartPollOptions = {}) => {
    const { isUp, intervalMs = 1500, initialDelayMs = 3000, deadlineMs = 120_000 } = opts;
    const my = ++gen.current;
    if (timer.current !== undefined) clearTimeout(timer.current);
    setState("waiting");
    const deadline = Date.now() + deadlineMs;
    let seenDown = false;
    const tick = async () => {
      try {
        const status = await get<SetupStatus>("/api/setup/status");
        if (my !== gen.current) return;
        if (isUp ? isUp(status) : seenDown) {
          window.location.reload();
          return;
        }
      } catch {
        // Unreachable — the restart in progress. This is the "down" half of
        // down-then-up.
        seenDown = true;
      }
      if (my !== gen.current) return;
      if (Date.now() > deadline) {
        setState("timeout");
        return;
      }
      timer.current = setTimeout(() => void tick(), intervalMs);
    };
    timer.current = setTimeout(() => void tick(), initialDelayMs);
  }, []);

  return { state, waiting: state === "waiting", timedOut: state === "timeout", begin };
}
