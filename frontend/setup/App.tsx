// The installer wizard. One linear story — welcome, network, disk, account,
// review, install, done — rendered identically on the machine's own display
// and on any LAN browser, both talking to the same fog-setup agent so every
// surface tells the same truth at the same moment.
//
// Booted from the on-disk recovery partition instead of the USB stick, the
// same app becomes the repair tool (agentMode === "repair").
//
// The house rule applies double here: never claim a fact before its data has
// landed. "Connected", "will be erased", "installed", "repaired" are claims.
import { useCallback, useEffect, useState } from "react";
import { get, post } from "../src/api";
import Logo from "../src/components/Logo";
import { Loading, errorText } from "../src/components/Async";
import type { AgentState, ExistingInstall, Inventory, KitInfo } from "./types";
import { IS_KIOSK, ScaleControl } from "./kiosk";
import { UnreachableScreen } from "./screens/Unreachable";
import { InstalledScreen } from "./screens/Installed";
import { WelcomeScreen } from "./screens/Welcome";
import { RestoreScreen } from "./screens/Restore";
import { DiskScreen } from "./screens/Disk";
import { IdentityScreen } from "./screens/Identity";
import { ReviewScreen } from "./screens/Review";
import { InstallScreen } from "./screens/Install";
import { DoneScreen } from "./screens/Done";
import { RepairHome } from "./screens/Repair";

type Screen =
  | "boot"
  | "unreachable"
  | "installed"
  | "welcome"
  | "restore"
  | "disk"
  | "identity"
  | "review"
  | "install"
  | "done"
  | "repair";

// The wizard's linear story per mode — one place to read (and change) the
// order, instead of mirrored ternaries in every onDone/onBack.
const FLOW: Record<"fresh" | "restore", Screen[]> = {
  fresh: ["welcome", "disk", "identity", "review"],
  restore: ["welcome", "restore", "disk", "review"],
};

export default function App() {
  const [screen, setScreen] = useState<Screen>("boot");
  const [inv, setInv] = useState<Inventory | null>(null);
  const [invError, setInvError] = useState("");
  // The finished install found on this machine's own disk, held from the
  // boot check: the "installed" screen keeps saying the same thing even if a
  // later inventory refresh can't re-read the marker.
  const [existing, setExisting] = useState<ExistingInstall | null>(null);
  const [agent, setAgent] = useState<AgentState | null>(null);

  // Wizard answers.
  const [mode, setMode] = useState<"fresh" | "restore">("fresh");
  const [kit, setKit] = useState<KitInfo | null>(null);
  const [disk, setDisk] = useState("");
  const [serverName, setServerName] = useState("fog");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [repairRun, setRepairRun] = useState(0);

  const stepAfter = (s: Screen) => FLOW[mode][FLOW[mode].indexOf(s) + 1];
  const stepBefore = (s: Screen) => FLOW[mode][FLOW[mode].indexOf(s) - 1];

  const refreshInventory = useCallback(async () => {
    try {
      const next = await get<Inventory>("/api/setup/inventory");
      setInv(next);
      setInvError("");
      return next;
    } catch (err) {
      setInvError(errorText(err, "Cannot reach the installer."));
      return null;
    }
  }, []);

  // Boot: ask the agent where things stand. A phone that joins (or reloads)
  // mid-install lands on the progress screen, not the welcome screen — and
  // an agent that doesn't answer gets its own screen, never a pristine
  // welcome that hides a possibly mid-erase machine.
  const bootCheck = useCallback(async () => {
    // Awaited, not fired off: what this machine's disk already holds is part
    // of deciding which screen opens.
    const found = await refreshInventory();
    try {
      const st = await get<AgentState>("/api/setup/state");
      setAgent(st);
      if (st.kit) setKit(st.kit);
      if (st.mode === "restore") setMode("restore");
      // A joining browser adopts the name the running/finished install
      // actually used — never this session's default.
      if (st.serverName) setServerName(st.serverName);
      if (st.agentMode === "repair") setScreen("repair");
      else if (st.phase === "installing" || st.phase === "failed") setScreen("install");
      else if (st.phase === "done") setScreen("done");
      // Nothing has run this boot, yet the machine's own disk already carries
      // a finished install: the USB stick was left in and the machine booted
      // from it again. Say that, instead of opening a wizard whose first real
      // act would be erasing the server that is already there.
      else if (found?.existing) {
        setExisting(found.existing);
        setScreen("installed");
      } else setScreen("welcome");
      return true;
    } catch {
      setScreen("unreachable");
      return false;
    }
  }, [refreshInventory]);

  useEffect(() => {
    bootCheck();
  }, [bootCheck]);

  // The machine line in the footer: just the machine's name, or what this
  // boot is for when the name hasn't landed.
  const subtitle =
    agent?.agentMode === "repair" ? "Recovery" : inv?.machine || "This machine";

  // The wordmark — the beacon and "fog" — over one veil on the shifted
  // ground; the wizard is one decision per screen, so nothing else is on it.
  const shell = (body: React.ReactNode, wide = false) => (
    <div className="relative flex min-h-screen items-center justify-center px-6 py-16">
      <div className={`relative z-10 w-full ${wide ? "max-w-2xl" : "max-w-lg"}`}>
        <h1 className="mb-10 flex items-center justify-center gap-3 text-2xl font-semibold text-ink">
          <Logo className="h-6" />
          fog
        </h1>
        <div className="rounded-xl border border-veil-edge bg-veil p-10">{body}</div>
        <div className="mt-6 space-y-1 text-center text-xs text-ink-muted">
          {agent?.version && <p>Fog installer {agent.version}</p>}
          <p>{subtitle}</p>
          <p>
            Accessible at <span className="font-mono text-ink-muted">http://fog.local</span>
          </p>
          {IS_KIOSK && (
            <div className="pt-3">
              <ScaleControl />
            </div>
          )}
        </div>
      </div>
    </div>
  );

  switch (screen) {
    case "boot":
      return shell(<Loading label="Looking at this machine…" />);
    case "unreachable":
      return shell(<UnreachableScreen retry={bootCheck} />);
    case "installed":
      return shell(
        <InstalledScreen existing={existing} onInstallAnyway={() => setScreen("welcome")} />,
      );
    case "repair":
      return shell(
        <RepairHome key={repairRun} agent={agent} onReset={() => setRepairRun((n) => n + 1)} />,
        true,
      );
    case "welcome":
      return shell(
        <WelcomeScreen
          onFresh={() => {
            setMode("fresh");
            setScreen("disk");
          }}
          onRestore={() => setScreen("restore")}
          // Arrived here from "this machine is already a Fog server" via
          // "install again": the way back is to that screen, not nowhere.
          onBack={existing ? () => setScreen("installed") : undefined}
        />,
      );
    case "restore":
      return shell(
        <RestoreScreen
          kit={kit}
          password={password}
          setPassword={setPassword}
          onLoaded={(info) => {
            setKit(info);
            setMode("restore");
            if (info.serverName) setServerName(info.serverName);
          }}
          onContinue={() => setScreen("disk")}
          onBack={() => setScreen("welcome")}
        />,
      );
    case "disk":
      return shell(
        <DiskScreen
          inv={inv}
          invError={invError}
          refresh={refreshInventory}
          selected={disk}
          onSelect={setDisk}
          onBack={() => setScreen(stepBefore("disk"))}
          onDone={() => setScreen(stepAfter("disk"))}
        />,
        true,
      );
    case "identity":
      return shell(
        <IdentityScreen
          serverName={serverName}
          setServerName={setServerName}
          username={username}
          setUsername={setUsername}
          password={password}
          setPassword={setPassword}
          onBack={() => setScreen(stepBefore("identity"))}
          onDone={() => setScreen(stepAfter("identity"))}
        />,
      );
    case "review":
      return shell(
        <ReviewScreen
          inv={inv}
          mode={mode}
          kit={kit}
          disk={disk}
          serverName={serverName}
          username={username}
          onBack={() => setScreen(stepBefore("review"))}
          onInstall={async () => {
            const target = inv?.disks.find((d) => d.path === disk);
            await post("/api/setup/install", {
              mode,
              disk,
              confirmDisk: disk,
              // Pin the identity of the disk that was actually shown and
              // confirmed — if device names reshuffled since, the agent
              // refuses instead of erasing the new occupant of the name.
              confirmModel: target?.model ?? "",
              confirmSizeBytes: target?.sizeBytes ?? 0,
              serverName,
              username: mode === "fresh" ? username : "",
              // Restores may carry the admin password too — the agent
              // verifies it against the kit and mirrors a unix account.
              password,
              timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
            });
            setScreen("install");
          }}
        />,
        true,
      );
    case "install":
      return shell(
        <InstallScreen
          onDone={(st) => {
            setAgent(st);
            setScreen("done");
          }}
          onRetry={() => {
            // Only a session still holding the answers can re-run the
            // install; a reloaded or second browser never knew the password
            // (or kit), so it walks the wizard again instead of landing on
            // a review screen full of blanks.
            const canReview =
              disk !== "" && (mode === "restore" ? kit !== null : username !== "" && password !== "");
            setScreen(canReview ? "review" : "welcome");
          }}
        />,
        true,
      );
    case "done":
      return shell(<DoneScreen agent={agent} />);
  }
}
