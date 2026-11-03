// Repair mode: the same app booted from the recovery partition.

import { useCallback, useEffect, useRef, useState } from "react";
import { get, post } from "../../src/api";
import Button from "../../src/components/Button";
import { errorText } from "../../src/components/Async";
import type { AgentState, RepairAction } from "../types";
import { IS_KIOSK } from "../kiosk";
import { ProgressList, useProgress } from "../progress";

// Titles and "keeps" promises come from the agent (AgentState.repairActions)
// so the screen can never promise something the engine doesn't do; only
// these purely presentational hints live here.
const REPAIR_HINTS: Record<string, string> = {
  fsck: "Start here after an unclean shutdown.",
  "rebuild-docker": "For apps that won't start after power loss.",
  "reinstall-os": "For a system that no longer boots on its own.",
};

export function RepairHome({ agent, onReset }: { agent: AgentState | null; onReset: () => void }) {
  const actions = agent?.repairActions ?? [];
  const [sub, setSub] = useState<"menu" | "running" | "done" | "failed" | "console">("menu");
  const [action, setAction] = useState<RepairAction | null>(null);
  const [error, setError] = useState("");
  const [restarting, setRestarting] = useState(false);
  // Whether this machine has a screen of its own to hand to the command
  // line; a headless box reached from a phone has nothing to drop.
  const [hasDisplay, setHasDisplay] = useState(false);
  useEffect(() => {
    get("/api/setup/display")
      .then(() => setHasDisplay(true))
      .catch(() => setHasDisplay(false));
  }, []);

  const onPhase = useCallback((st: AgentState) => {
    if (st.phase === "done") setSub("done");
    if (st.phase === "failed") {
      setError(st.error || "The repair failed.");
      setSub("failed");
    }
  }, []);
  const { steps, failed } = useProgress(onPhase);

  const start = async (a: RepairAction) => {
    setAction(a);
    setError("");
    try {
      await post("/api/setup/repair", { action: a.id });
      setSub("running");
    } catch (err) {
      setError(errorText(err, "Could not start the repair."));
    }
  };

  const backToMenu = async () => {
    await post("/api/setup/repair/reset").catch(() => {});
    // Remount (via the key in App) for a clean event stream — no page
    // reload needed.
    onReset();
  };

  if (sub === "running" || sub === "failed") {
    return (
      <div className="space-y-6">
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">
          {sub === "failed" ? "The repair hit a problem" : action?.title}
        </h2>
        <ProgressList steps={steps} failed={failed} />
        {sub === "failed" && (
          <div className="space-y-3">
            <div className="rounded-lg border border-warn/30 bg-warn/10 p-4">
              <p className="text-sm text-warn">{error}</p>
              <p className="mt-1 text-xs text-warn">
                Your data partition was not touched.{" "}
                <a href="/api/setup/diagnostics" className="underline">
                  Download diagnostics
                </a>{" "}
                to get help.
              </p>
            </div>
            <Button variant="primary" className="w-full" onClick={backToMenu}>
              Back to recovery menu
            </Button>
          </div>
        )}
      </div>
    );
  }

  if (sub === "console") {
    return <ConsoleScreen onBack={() => setSub("menu")} />;
  }

  // Nothing chosen, machine going down: the kiosk dies with it, and a LAN
  // browser is told where it will find Fog once the disk's own boot entry
  // comes up — or this very menu again, if that still fails.
  if (restarting) {
    return (
      <div className="space-y-6 text-center">
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">Restarting…</h2>
        <p className="text-sm text-ink-muted">
          The machine is restarting into Fog without any repair having run. Give it a few
          minutes; if it lands here again, come back to this menu and try the first option.
        </p>
      </div>
    );
  }

  if (sub === "done") {
    return (
      <div className="space-y-6 text-center">
        <span className="text-4xl">✓</span>
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">{action?.title}: complete</h2>
        <p className="text-sm text-ink-muted">
          Restart the machine to boot back into Fog. If it still doesn&apos;t come up, boot
          here again and try the next option down the list.
        </p>
        <div className="flex gap-3">
          <Button className="flex-1" onClick={backToMenu}>
            More repairs
          </Button>
          <Button
            variant="primary"
            className="flex-1"
            onClick={() => post("/api/setup/reboot").catch(() => {})}
          >
            Restart into Fog
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-5">
      <div>
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">Fog Recovery</h2>
        <p className="mt-1 text-sm text-ink-muted">
          This machine started in recovery mode. <strong className="text-ink">Your
          apps&apos; data is intact</strong> — these tools fix the system around it, safest
          first.
        </p>
      </div>
      <div className="space-y-3">
        {actions.map((a) => (
          <button
            key={a.id}
            onClick={() => start(a)}
            className="w-full rounded-lg border border-veil-edge bg-veil p-4 text-left transition-colors hover:bg-fog-800"
          >
            <p className="text-sm font-medium text-ink">{a.title}</p>
            {REPAIR_HINTS[a.id] && <p className="mt-1 text-xs text-ink-muted">{REPAIR_HINTS[a.id]}</p>}
            <p className="mt-1 text-xs text-accent">Keeps: {a.keeps}</p>
          </button>
        ))}
        {hasDisplay && (
          <button
            onClick={() => setSub("console")}
            className="w-full rounded-lg border border-dashed border-veil-edge p-4 text-left transition-colors hover:bg-fog-800"
          >
            <p className="text-sm font-medium text-ink">Drop to the command line</p>
            <p className="mt-1 text-xs text-ink-muted">
              Safe mode: closes this screen and leaves a root shell on the machine&apos;s console.
              Nothing runs until you type it.
            </p>
            <p className="mt-1 text-xs text-accent">Keeps: Everything — nothing on the disk is mounted or changed.</p>
          </button>
        )}
      </div>
      {error && <p className="text-sm text-warn">{error}</p>}
      <Button
        className="w-full"
        onClick={async () => {
          setError("");
          try {
            await post("/api/setup/reboot");
          } catch (err) {
            setError(errorText(err, "Couldn't restart the machine."));
            return;
          }
          setRestarting(true);
        }}
      >
        Restart without repairing
      </Button>
      <p className="text-center text-xs text-ink-muted">
        Also here from any browser: <span className="font-mono text-ink-muted">http://fog.local</span>
        {" · "}
        <a href="/api/setup/diagnostics" className="underline hover:text-ink-muted">
          download diagnostics
        </a>
      </p>
    </div>
  );
}

// The safe-mode exit: the machine's screen stops drawing this menu and shows
// the root shell that has been on its console all along. On the kiosk the
// page dies with the kiosk; from a LAN browser the page stays and can bring
// the screen back.
function ConsoleScreen({ onBack }: { onBack: () => void }) {
  const [state, setState] = useState<"ask" | "dropping" | "dropped">("ask");
  const [error, setError] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const hand = async (on: boolean) => {
    setError("");
    try {
      await post("/api/setup/console", { on });
    } catch (err) {
      setError(errorText(err, on ? "Couldn't close the screen." : "Couldn't bring the screen back."));
      return;
    }
    if (!on) {
      setState("ask");
      return;
    }
    if (!IS_KIOSK) {
      setState("dropped");
      return;
    }
    // This page is the kiosk: the stop takes it with it. Still here well
    // after that should have happened means it didn't.
    setState("dropping");
    timer.current = setTimeout(() => {
      setState("ask");
      setError("The screen didn't close. The console may already be showing on another display.");
    }, 20_000);
  };

  if (state === "dropping") {
    return <p className="text-center text-sm text-ink-muted">Closing this screen…</p>;
  }

  if (state === "dropped") {
    return (
      <div className="space-y-6">
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">
          The machine&apos;s screen is now a command line
        </h2>
        <p className="text-sm text-ink-muted">
          A root shell on the recovery system is on the console. Fog&apos;s partitions are labelled{" "}
          <span className="font-mono text-ink">fog-root</span> and{" "}
          <span className="font-mono text-ink">fog-data</span>; nothing is mounted until you
          mount it. Typing <span className="font-mono text-ink">systemctl start fog-kiosk</span>{" "}
          there, or the button below, brings this menu back on the screen.
        </p>
        {error && <p className="text-sm text-warn">{error}</p>}
        <div className="flex gap-3">
          <Button className="flex-1" onClick={onBack}>
            Back to recovery menu
          </Button>
          <Button variant="primary" className="flex-1" onClick={() => hand(false)}>
            Bring the screen back
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">
        Drop to the command line
      </h2>
      <div className="space-y-3 text-sm text-ink-muted">
        <p>
          This closes the recovery screen on the machine&apos;s display and leaves a{" "}
          <strong className="text-ink">root shell</strong> on its console. It is safe mode:
          nothing from this machine&apos;s disk is mounted, nothing runs, and nothing changes
          until you type it.
        </p>
        <p>
          The way back is typed at that shell:{" "}
          <span className="font-mono text-ink">systemctl start fog-kiosk</span> for this
          menu, <span className="font-mono text-ink">reboot</span> for Fog itself.
          {!IS_KIOSK && " This browser keeps a button for it too."}
        </p>
      </div>
      {error && <p className="text-sm text-warn">{error}</p>}
      <div className="flex gap-3">
        <Button className="flex-1" onClick={onBack}>
          Cancel
        </Button>
        <Button variant="danger" className="flex-1" onClick={() => hand(true)}>
          Drop to the command line
        </Button>
      </div>
    </div>
  );
}
