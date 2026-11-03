import { useEffect, useState } from "react";
import { post } from "../../src/api";
import Button from "../../src/components/Button";
import { Loading } from "../../src/components/Async";
import type { AgentState } from "../types";
import { IS_KIOSK } from "../kiosk";

export function DoneScreen({ agent }: { agent: AgentState | null }) {
  const [rebooting, setRebooting] = useState(false);
  const [waiting, setWaiting] = useState(false);
  // The follow can genuinely fail even though the server is fine: some
  // clients can't resolve the new name over mDNS, and DHCP may hand the
  // installed system a different address than the live environment had.
  // After a couple of minutes, say so and hand over the manual routes.
  const [slow, setSlow] = useState(false);
  // The one-shot claim token, taken from the agent exactly once, right
  // before the restart — never broadcast in /api/setup/state where anyone
  // on the LAN could scrape it. Kept in sessionStorage so a reload while
  // waiting doesn't lose the handoff.
  const [claimToken, setClaimToken] = useState(() => sessionStorage.getItem("fog-claim") || "");
  const result = agent?.result;
  const restored = agent?.mode === "restore";
  // The kiosk on the machine's own display dies with the reboot — it must
  // never consume the token a LAN browser could actually use.
  const isKiosk = IS_KIOSK;

  // After the reboot, hunt for the installed server and hand this browser
  // over. Fresh installs carry the one-shot token (signed in on arrival);
  // restores land on the restored server's own login.
  useEffect(() => {
    if (!waiting || !result) return;
    const t = setInterval(async () => {
      for (const base of result.urls) {
        try {
          const res = await fetch(`${base}/api/setup/status`, {
            signal: AbortSignal.timeout(2000),
          });
          if (res.ok) {
            window.location.href = claimToken
              ? `${base}/api/setup/claim?token=${encodeURIComponent(claimToken)}`
              : base;
            return;
          }
        } catch {
          /* not up yet — keep listening */
        }
      }
    }, 3000);
    return () => clearInterval(t);
  }, [waiting, result, claimToken]);

  useEffect(() => {
    if (!waiting) return;
    const t = setTimeout(() => setSlow(true), 150_000);
    return () => clearTimeout(t);
  }, [waiting]);

  return (
    <div className="space-y-6 text-center">
      <div>
        <span className="text-4xl">🎉</span>
        <h2 className="mt-3 text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">
          {restored ? "Your server is restored" : "Fog is installed"}
        </h2>
        <p className="mt-2 text-sm text-ink-muted">
          {waiting
            ? isKiosk
              ? "Restarting into Fog. This screen goes dark now — remove the USB stick, and the console will show your server's address when it's back."
              : restored
                ? "When the server comes back, this page will take you to its sign-in — use your existing password. Then re-restore each app from its backup on the dashboard."
                : "When the server comes back, this page will take you straight to your dashboard."
            : "One restart to go. Remove the USB stick when the screen goes dark."}
        </p>
      </div>
      {waiting ? (
        <div className="space-y-4">
          <Loading label="Waiting for your server to come back…" className="justify-center" />
          {slow && result && (
            <div className="rounded-lg border border-veil-edge bg-veil p-4 text-left">
              <p className="text-sm text-ink">
                This is taking longer than usual. The server may be up even though this
                page can&apos;t see it — try these in a new tab:
              </p>
              <ul className="mt-2 space-y-1">
                {result.urls.map((u) => (
                  <li key={u}>
                    <a href={u} className="font-mono text-xs text-ink-muted underline">
                      {u}
                    </a>
                  </li>
                ))}
              </ul>
              <p className="mt-2 text-xs text-ink-muted">
                If neither answers, find the machine&apos;s address in your router&apos;s
                client list and open http://&lt;that address&gt;:8080 — some networks and
                phones can&apos;t resolve .local names, and the address can change across
                the restart.
              </p>
            </div>
          )}
        </div>
      ) : (
        <Button
          variant="primary"
          className="w-full py-2.5"
          disabled={rebooting}
          onClick={async () => {
            setRebooting(true);
            // Take the claim token before the agent goes down. It is handed
            // out once — to the surface driving the restart. Losing the race
            // just means landing on the sign-in page instead.
            if (!restored && !isKiosk && !claimToken) {
              try {
                const res = await post<{ claimToken: string }>("/api/setup/claim-token");
                setClaimToken(res.claimToken);
                sessionStorage.setItem("fog-claim", res.claimToken);
              } catch {
                /* already taken by another surface */
              }
            }
            try {
              await post("/api/setup/reboot");
            } catch {
              /* the agent may die mid-response as the machine goes down */
            }
            setWaiting(true);
          }}
        >
          Restart into Fog
        </Button>
      )}
      {result && (
        <p className="text-xs text-ink-muted">
          Your dashboard will live at{" "}
          <span className="font-mono text-ink-muted">{result.urls[0]}</span>
        </p>
      )}
    </div>
  );
}
