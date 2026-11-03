// The shell a household member sees: the chat and nothing else. A slim bar
// names the household and the person, opens their devices, and signs out.
// No sidebar — there is no fog to administer from here.

import { useState } from "react";
import { Outlet } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { post } from "../api";
import { meQuery, personMeQuery } from "../queries";
import Logo from "./Logo";
import DevicesPanel from "../pages/chat/DevicesPanel";
import { useChat } from "../lib/chatClient";

export default function MemberLayout({ household, handle }: { household: string; handle: string }) {
  const qc = useQueryClient();
  const { client, phase } = useChat();
  const [devices, setDevices] = useState(false);

  async function signOut() {
    client?.close();
    await post("/api/auth/logout").catch(() => {});
    await qc.invalidateQueries({ queryKey: meQuery.queryKey });
    await qc.invalidateQueries({ queryKey: personMeQuery.queryKey });
  }

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-30 flex h-14 items-center gap-3 border-b border-hair bg-fog-950/70 px-4 backdrop-blur sm:px-6">
        <Logo className="h-5" />
        <span className="text-[15px] font-semibold text-ink">{household}</span>
        <span className="text-hint text-ink-muted">chat</span>
        <div className="ml-auto flex items-center gap-3 text-sm">
          <span className="font-mono text-xs text-ink-muted">@{handle}</span>
          <button
            onClick={() => setDevices(true)}
            disabled={phase !== "ready"}
            className="rounded-md border border-edge px-2 py-1 text-xs text-ink-muted hover:bg-hover disabled:opacity-50">
            Devices
          </button>
          <button onClick={signOut} className="rounded-md px-2 py-1 text-xs text-ink-muted hover:bg-hover hover:text-ink">
            Sign out
          </button>
        </div>
      </header>
      <main className="min-w-0">
        <Outlet />
      </main>
      {devices && client && <DevicesPanel client={client} onClose={() => setDevices(false)} />}
    </div>
  );
}
