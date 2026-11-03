// One ChatClient per browser, in one tab at a time: the SDK takes a Web
// Lock for the device's keys, and any other tab says so and waits.
// One ChatClient per browser tab. The client holds MLS state and decrypted
// messages, so it must survive navigation: it lives at module scope and is
// initialized once. Components subscribe through useChat and re-render on
// the client's events by bumping a counter — the client is the state, React
// is the view of it.

import { useEffect, useState } from "react";
import { ChatClient, CryptoUnavailableError, DeviceInUseError, IndexedDBStorage, webCryptoAvailable } from "@fog/chat-sdk";

type ChatPhase = "keys" | "sync" | "ready" | "error";

interface Boot {
  client: ChatClient | null;
  phase: ChatPhase;
  error: string;
  // status is the stream: online once the WebSocket is open.
  status: "online" | "offline";
  // otherDevices is how many devices besides this one the person holds on
  // the server — what decides whether an unverified device waits for an
  // approval or is simply the first.
  otherDevices: number;
  // insecure: the page was opened over plain http from another host, so the
  // browser withholds WebCrypto and nothing here can run.
  insecure: boolean;
  // identityChanged holds people whose identity key moved since this
  // device pinned it: the "verify again" moment.
  identityChanged: Set<string>;
}

const DB = "fog-chat";
const boot: Boot = { client: null, phase: "keys", error: "", status: "offline", otherDevices: 0, identityChanged: new Set(), insecure: false };
// The client's busy events. A component names the ones it draws from; the
// bootstrap's own changes (phase, stream status, identity, me, errors)
// reach every watcher.
export type ChatEvent = "rooms" | "messages" | "presence" | "typing";
const ALL: readonly ChatEvent[] = ["rooms", "messages", "presence", "typing"];

const watchers = new Set<(kind?: ChatEvent) => void>();
let starting: Promise<void> | null = null;

function notify(kind?: ChatEvent) {
  watchers.forEach((w) => w(kind));
}

// start runs the bootstrap once: keys first (identity, device, key
// packages), then the sync and the stream. Each step is a phase the page
// can say out loud, so a slow first run never looks stuck.
function start(): Promise<void> {
  if (starting) return starting;
  starting = run();
  return starting;
}

async function run(): Promise<void> {
  try {
    if (!webCryptoAvailable()) {
      boot.insecure = true;
      throw new CryptoUnavailableError();
    }
    const storage = await IndexedDBStorage.open(DB);
    const client = new ChatClient({ storage });
    boot.client = client;
    boot.phase = "keys";
    boot.error = "";
    client.addEventListener("status", (s) => {
      boot.status = s === "online" ? "online" : "offline";
      notify();
    });
    for (const kind of ALL) client.addEventListener(kind, () => notify(kind));
    client.addEventListener("me", () => notify());
    client.addEventListener("identity", (d) => {
      const ev = d as { addr?: string; changed?: boolean; verified?: boolean };
      if (ev.addr && ev.changed) boot.identityChanged.add(ev.addr);
      if (ev.addr && ev.verified) boot.identityChanged.delete(ev.addr);
      notify();
    });
    client.addEventListener("error", (e) => {
      console.warn("chat", e);
      notify();
    });
    const me = await client.init();
    try {
      const server = await client.api.me();
      boot.otherDevices = server.devices.filter((d) => d.deviceKey !== me.deviceKey).length;
    } catch {
      boot.otherDevices = 0;
    }
    boot.phase = "sync";
    notify();
    await client.connect();
    boot.phase = "ready";
    notify();
  } catch (e) {
    boot.phase = "error";
    boot.error = e instanceof Error ? e.message : String(e);
    notify();
    // Another tab holds this device's keys: this one waits its turn, and
    // takes over (by starting afresh) once that tab is gone.
    if (e instanceof DeviceInUseError) {
      const locks = (navigator as Navigator & { locks?: LockManager }).locks;
      locks?.request("fog-chat-device:" + DB, () => {
        location.reload();
        return new Promise(() => {});
      });
    }
  }
}

// restoreFromPhrase replaces whatever this browser holds with the person's
// backup: the running client is closed, the store cleared and seeded from
// the backup, and the bootstrap runs again so the restored device binds to
// its session and rejoins its rooms.
export async function restoreFromPhrase(phrase: string): Promise<{ addr: string; messages: number }> {
  boot.client?.close();
  boot.client = null;
  boot.phase = "keys";
  boot.error = "";
  notify();
  const storage = await IndexedDBStorage.open(DB);
  await storage.clear();
  const result = await ChatClient.restoreFromPhrase({ storage }, phrase);
  starting = run();
  await starting;
  return result;
}

// refreshOtherDevices re-reads the person's device list, after an approval
// or a revoke elsewhere changed it.
export async function refreshOtherDevices() {
  const client = boot.client;
  if (!client) return;
  try {
    const server = await client.api.me();
    boot.otherDevices = server.devices.filter((d) => d.deviceKey !== client.me().deviceKey).length;
    notify();
  } catch {
    /* keep the last count */
  }
}

// useChat hands a component the shared client and re-renders it whenever
// the client says something it draws from changed: the bootstrap's state
// always, and of the busy events only those in `events`. A typing ping in
// some room then costs a render only where typing is shown. `version` is
// only a dependency hook.
export function useChat(events: readonly ChatEvent[] = ALL) {
  const [version, setVersion] = useState(0);
  const key = events.join(",");
  useEffect(() => {
    const wanted = new Set(key.split(","));
    const w = (kind?: ChatEvent) => {
      if (!kind || wanted.has(kind)) setVersion((v) => v + 1);
    };
    watchers.add(w);
    start();
    return () => {
      watchers.delete(w);
    };
  }, [key]);
  return {
    client: boot.client,
    phase: boot.phase,
    error: boot.error,
    status: boot.status,
    otherDevices: boot.otherDevices,
    insecure: boot.insecure,
    identityChanged: boot.identityChanged,
    version,
  };
}
