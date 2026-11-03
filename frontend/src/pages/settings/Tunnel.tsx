// Settings › Network › Cloudflare Tunnel: fogd installs and supervises
// cloudflared so selected services can be reached from the internet
// without port forwarding.

import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { post, put } from "../../api";
import { errorText } from "../../components/Async";
import Button from "../../components/Button";
import Field from "../../components/Field";
import { StatusLine } from "../../components/Section";
import { useApps } from "../../lib/useApps";
import { paths } from "../../lib/paths";
import { SettingsPage } from "./Frame";
import { settingsQuery, tunnelQuery } from "../../queries";

// Cloudflare Tunnel: fogd installs and supervises cloudflared so selected
// services can be reached from the internet without port forwarding.
export default function Tunnel() {
  const qc = useQueryClient();
  const [token, setToken] = useState("");
  const [error, setError] = useState("");

  const status = useQuery({ ...tunnelQuery, refetchInterval: 5000 });
  const st = status.data;

  const start = useMutation({
    mutationFn: async () => {
      if (token.trim()) {
        await put("/api/system/secrets", { key: "cloudflare_tunnel_token", value: token.trim() });
        setToken("");
      }
      return post("/api/tunnel/start");
    },
    onSuccess: () => {
      setError("");
      // Awaited: "Starting…" holds until the tunnel's own status confirms it.
      return qc.invalidateQueries({ queryKey: tunnelQuery.queryKey });
    },
    onError: (e) => setError(errorText(e, "Start failed")),
  });
  const stop = useMutation({
    mutationFn: () => post("/api/tunnel/stop"),
    onSuccess: () => qc.invalidateQueries({ queryKey: tunnelQuery.queryKey }),
    onError: (e) => setError(errorText(e, "Stop failed")),
  });

  // "not installed" is a conclusion; before the status arrives, say so.
  const statusText = !st
    ? status.isError
      ? "status unavailable"
      : "checking…"
    : st.installing
      ? "installing cloudflared…"
      : st.running
        ? `running (pid ${st.pid}${st.restarts ? `, ${st.restarts} restarts` : ""})`
        : st.installed
          ? "stopped"
          : "not installed";

  const failing = !st?.running && !!(st?.lastError || st?.installError);

  return (
    <SettingsPage
      status={
        <StatusLine tone={st?.running ? "ok" : failing ? "warn" : "idle"}>
          {st?.tokenConfigured || !st ? statusText : "Not set up — no connector token saved."}
        </StatusLine>
      }
      description={
        <>
          Reach chosen services from the internet without opening ports — the route to take when the domain
          wizard reports carrier-grade NAT, or when you'd rather not forward a port at all: fogd runs
          Cloudflare's connector as a supervised process. Create a tunnel in the{" "}
          <a href="https://one.dash.cloudflare.com" target="_blank" rel="noreferrer" className="text-ink-muted underline">
            Zero Trust dashboard
          </a>{" "}
          (Networks → Tunnels), paste its token here, and configure routes there.
        </>
      }
    >
      <div className="max-w-lg space-y-4">
        <Field label="Tunnel token" type="password" value={token} onChange={(e) => setToken(e.target.value)}
          placeholder={!st ? "checking for a saved token…" : st.tokenConfigured ? "•••••• (saved — enter only to replace)" : "paste the connector token"} />
        <div className="flex items-center gap-3">
          {st?.running ? (
            <Button onClick={() => stop.mutate()} disabled={stop.isPending}>Stop tunnel</Button>
          ) : (
            <Button variant="primary" onClick={() => start.mutate()}
              disabled={start.isPending || !st || (!st.tokenConfigured && !token.trim())}>
              {start.isPending ? "Starting…" : "Start tunnel"}
            </Button>
          )}
          <span className={`text-sm ${st?.running ? "text-accent" : "text-ink-muted"}`}>{statusText}</span>
        </div>
        {(st?.lastError || st?.installError) && !st?.running && (
          <p className="break-words font-mono text-xs text-warn">{st.installError || st.lastError}</p>
        )}
        {error && <p className="text-sm text-warn">{error}</p>}
      </div>
      {st?.tokenConfigured && <TunnelGuide service={st.service ?? ""} />}
    </SettingsPage>
  );
}

// Once a token is saved, the natural next question is "how do I actually
// reach my apps through this?" — and the answer lives in Cloudflare's
// dashboard, not here. This guide bridges that gap, using the operator's
// real domain and app names so the steps can be followed verbatim.
function TunnelGuide({ service }: { service: string }) {
  const settings = useQuery(settingsQuery);
  const apps = useApps();

  // No hostnames until both the domain and the app list have landed —
  // a guide showing "yourapp.undefined" teaches the wrong lesson.
  if (!settings.data || !apps.data) return null;

  const domain = settings.data.base_domain;
  if (!domain) {
    return (
      <div className="max-w-2xl space-y-2 rounded-md border border-edge p-4">
        <h3 className="text-sm font-medium text-ink">Reaching your apps through the tunnel</h3>
        <p className="text-xs text-ink-muted">
          Tunnel hostnames reuse the per-app names fog's proxy already serves, which need a base
          domain. Set one up under <Link to={paths.settings("network/domain")} className="underline">Domain & HTTPS</Link> first,
          then come back here for the routing steps.
        </p>
      </div>
    );
  }

  // Straight from the status rows, which already resolve custom route hosts —
  // rebuilding "<name>.<domain>" here would print the wrong name for any app
  // installed on a custom address.
  const hosts = ["fog." + domain, ...apps.data.flatMap((a) => a.hosts ?? [])];
  return (
    <div className="max-w-2xl space-y-3 rounded-md border border-edge p-4">
      <h3 className="text-sm font-medium text-ink">Reaching your apps through the tunnel</h3>
      <p className="text-xs text-ink-muted">
        Fog keeps the connector alive; which hostnames travel through it is configured in the{" "}
        <a href="https://one.dash.cloudflare.com" target="_blank" rel="noreferrer" className="text-ink-muted underline">
          Zero Trust dashboard
        </a>
        . For each app you want reachable from outside your network:
      </p>
      <ol className="list-decimal space-y-1.5 pl-5 text-xs text-ink-muted">
        <li>Open Networks → Tunnels → your tunnel → <span className="text-ink-muted">Public Hostnames</span> → Add a public hostname.</li>
        <li>
          Enter the app's subdomain with domain <span className="font-mono text-ink-muted">{domain}</span> — the same
          hostname fog's proxy serves on your LAN (see the list below).
        </li>
        <li>
          Set the service type to <span className="text-ink-muted">UNIX</span> and its path to{" "}
          <span className="break-all font-mono text-ink-muted">{service.replace(/^unix:/, "")}</span>. That's fog's
          proxy's tunnel socket: only the connector fog runs can reach it, it recognizes the hostname, routes to the
          right app, and knows each visitor by the address Cloudflare names. (Not https://localhost:443 — there every
          tunnel visitor looks like this machine, and fog refuses to sign them in.)
        </li>
      </ol>
      <p className="text-xs text-ink-muted">
        Cloudflare then creates a DNS record sending that name through the tunnel, and the app is reachable from
        anywhere — no ports opened. Hostnames fog serves right now:
      </p>
      <p className="break-words font-mono text-xs text-ink-muted">{hosts.join("  ")}</p>
      <ul className="list-disc space-y-1.5 pl-5 text-xs text-ink-muted">
        <li>
          Don't add a wildcard (<span className="font-mono">*.{domain}</span>) hostname to the tunnel — fog manages the
          wildcard DNS record for LAN access and will reclaim it.
        </li>
        <li>Once a hostname routes through the tunnel, devices at home reach it via Cloudflare too — expect an extra round trip.</li>
        <li>
          Apps behind fog's login keep it through the tunnel. For anything sensitive, also add a Cloudflare Access
          application for the hostname in the same dashboard — Cloudflare then checks identity before traffic reaches
          this machine.
        </li>
      </ul>
    </div>
  );
}
