// Settings › Network › Away access. The domain wizard's `wan` target needs a
// forwarded port and a public address; this is the other answer, and the one
// that needs nothing from the router: fog joins the operator's own tailnet
// and advertises a single route to its own LAN address, so `*.<domain>`
// keeps resolving to exactly what it resolves to at home and connects from
// anywhere. Nothing about DNS changes, which is the whole trick.
//
// Two of the three steps happen outside fog — approving the route, and
// installing Tailscale on the away device — so this section's real job is
// saying which of them is still outstanding, and never guessing.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { post, put, type VpnStatus } from "../../api";
import { errorText } from "../../components/Async";
import Button from "../../components/Button";
import Field from "../../components/Field";
import { StatusLine } from "../../components/Section";
import { SettingsPage } from "./Frame";
import { settingsQuery, vpnQuery } from "../../queries";

const ADMIN_MACHINES = "https://login.tailscale.com/admin/machines";
const ADMIN_KEYS = "https://login.tailscale.com/admin/settings/keys";

export default function Away() {
  const qc = useQueryClient();
  const [key, setKey] = useState("");
  const [error, setError] = useState("");

  const status = useQuery({ ...vpnQuery, refetchInterval: 5000 });
  const st = status.data;

  const start = useMutation({
    mutationFn: async () => {
      // A key pasted over an existing login is a request to move fog to
      // that tailnet, which `tailscale up` won't do on its own — so saving
      // one while already joined asks for the rejoin explicitly.
      const reauth = !!key.trim() && !!st?.loggedIn;
      if (key.trim()) {
        await put("/api/system/secrets", { key: "tailscale_auth_key", value: key.trim() });
        setKey("");
      }
      return post("/api/vpn/start", { reauth });
    },
    onSuccess: () => {
      setError("");
      return qc.invalidateQueries({ queryKey: vpnQuery.queryKey });
    },
    onError: (e) => setError(errorText(e, "Start failed")),
  });
  const stop = useMutation({
    mutationFn: () => post("/api/vpn/stop"),
    onSuccess: () => qc.invalidateQueries({ queryKey: vpnQuery.queryKey }),
    onError: (e) => setError(errorText(e, "Stop failed")),
  });

  const snap = st?.snapshot;
  // Everything below distinguishes "we haven't looked yet" from "we looked
  // and it isn't working". Reporting the first as the second sends the
  // operator to the admin console to fix something that may be fine.
  const statusText = !st
    ? status.isError
      ? "status unavailable"
      : "checking…"
    : st.installing
      ? "installing tailscale…"
      : !st.running
        ? st.installed || st.keyConfigured
          ? "stopped"
          : "not set up"
        : !st.statusKnown
          ? "starting…"
          : snap?.backendState === "NeedsLogin"
            ? "needs sign-in"
            : !snap?.online
              ? "connecting…"
              : snap.routeApproved
                ? `connected as ${snap.dnsName || "this machine"} — route ${st.route} approved`
                : `connected — route ${st.route} is waiting for approval`;

  const working = !!(st?.running && st.statusKnown && snap?.online && snap.routeApproved);
  const halfDone = !!(st?.running && st.statusKnown && !working);
  const failing = !!(!st?.running && (st?.lastError || st?.installError || st?.upError));

  return (
    <SettingsPage
      status={
        <StatusLine tone={working ? "ok" : halfDone || failing ? "warn" : "idle"}>
          {st && !st.keyConfigured && !st.loggedIn && !st.running ? "Not set up — no auth key saved." : statusText}
        </StatusLine>
      }
      description={
        <>
          Reach this box from anywhere with the same hostnames you use at home — no port forwarded, and it works
          behind carrier-grade NAT. Fog joins your{" "}
          <a href="https://tailscale.com" target="_blank" rel="noreferrer" className="text-ink-muted underline">
            Tailscale
          </a>{" "}
          network and advertises one route: its own LAN address. Your DNS doesn't change, so{" "}
          <span className="font-mono">*.yourdomain</span> resolves the same on the road as on the sofa.
        </>
      }
    >
      <div className="max-w-lg space-y-4">
        <Field
          label="Auth key"
          type="password"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          placeholder={
            !st
              ? "checking for a saved key…"
              : st.loggedIn
                ? "•••••• (already joined — enter a key only to move tailnets)"
                : st.keyConfigured
                  ? "•••••• (saved — enter only to replace)"
                  : "paste an auth key"
          }
          hint={
            <>
              Generate one under{" "}
              <a href={ADMIN_KEYS} target="_blank" rel="noreferrer" className="underline">
                Settings → Keys
              </a>{" "}
              in the Tailscale admin console. It's used once to join and then only stored.
            </>
          }
        />
        <div className="flex items-center gap-3">
          {st?.running ? (
            <Button onClick={() => stop.mutate()} disabled={stop.isPending}>
              {stop.isPending ? "Turning off…" : "Turn off"}
            </Button>
          ) : (
            <Button
              variant="primary"
              onClick={() => start.mutate()}
              disabled={start.isPending || !st || (!st.keyConfigured && !st.loggedIn && !key.trim())}
            >
              {start.isPending ? "Starting…" : "Turn on"}
            </Button>
          )}
          <span className={`text-sm ${working ? "text-accent" : "text-ink-muted"}`}>{statusText}</span>
        </div>
        {st?.routeError && !st.route && (
          <p className="text-sm text-warn">Nothing to advertise yet: {st.routeError}</p>
        )}
        {(st?.installError || st?.lastError || st?.upError) && !working && (
          <p className="break-words font-mono text-xs text-warn">
            {st.installError || st.upError || st.lastError}
          </p>
        )}
        {st?.running && st.statusKnown && snap?.routeApproved && (
          <p className="text-xs text-ink-muted">
            Devices that have accepted this route reach fog through the tailnet even at home, so turning this off
            withdraws the route first and waits for that to reach them — a second or two — before stopping. Without
            it they'd keep sending fog's traffic into a tunnel that had gone away.
          </p>
        )}
        {st?.notes?.map((n) => (
          <p key={n} className="text-xs text-warn">{n}</p>
        ))}
        {snap?.health?.map((h) => (
          <p key={h} className="text-xs text-ink-muted">{h}</p>
        ))}
        {error && <p className="text-sm text-warn">{error}</p>}
      </div>
      {snap?.backendState === "NeedsLogin" && snap.authUrl && (
        <p className="mt-4 text-sm text-ink-muted">
          The daemon is waiting to be signed in.{" "}
          <a href={snap.authUrl} target="_blank" rel="noreferrer" className="text-ink-muted underline">
            Open the sign-in link
          </a>{" "}
          — or paste a fresh auth key above and turn it on again.
        </p>
      )}
      <VpnGuide st={st} />
      <RoutesOverride st={st} />
    </SettingsPage>
  );
}

// The two steps fog can't take for you, written with the real route in them
// so they can be followed verbatim.
function VpnGuide({ st }: { st?: VpnStatus }) {
  const settings = useQuery(settingsQuery);
  // The route and the domain are the two things that make these steps
  // copyable; without them the guide would teach a placeholder.
  if (!st || !settings.data) return null;
  const route = st.route;
  const domain = settings.data.base_domain;
  const approved = st.statusKnown && st.snapshot?.routeApproved;

  return (
    <div className="mt-4 max-w-2xl space-y-3 rounded-md border border-edge p-4">
      <h3 className="text-sm font-medium text-ink">Getting there from the road</h3>
      <ol className="list-decimal space-y-1.5 pl-5 text-xs text-ink-muted">
        <li>
          <span className={approved ? "text-ink-muted line-through" : undefined}>
            Approve the route{route ? <> <span className="font-mono text-ink-muted">{route}</span></> : null} for this
            machine in{" "}
            <a href={ADMIN_MACHINES} target="_blank" rel="noreferrer" className="text-ink-muted underline">
              Machines
            </a>{" "}
            → this machine → Edit route settings.
          </span>
          {approved && <span className="ml-2 text-accent">done</span>}
          {!approved && st.statusKnown && (
            <span className="ml-2 text-warn">outstanding — nothing will connect until this is ticked</span>
          )}
        </li>
        <li>
          Install Tailscale on the devices you want to reach fog from, and sign them into the same tailnet. Nothing
          needs configuring on them — the route arrives automatically.
        </li>
        <li>
          Open{" "}
          <span className="font-mono text-ink-muted">{domain ? `fog.${domain}` : "your fog hostname"}</span> with Wi-Fi
          off. Same name, same certificate, same page.
        </li>
      </ol>
      <ul className="list-disc space-y-1.5 pl-5 text-xs text-ink-muted">
        <li>
          A device can only be on one tailnet at a time. If you use Tailscale for work, switch profiles in its menu
          when you want to reach fog — the work tunnel comes back the same way.
        </li>
        <li>
          Fog takes neither DNS nor routes from the tailnet, so joining doesn't change how this machine reaches the
          internet — only how you reach it.
        </li>
        <li>
          Once a device accepts the route, it reaches fog through the tailnet even on your own wifi — the host route
          wins over its LAN route. That's harmless (the tunnel takes a direct path across the LAN), but it does mean
          fog's connector is the only way in from that device, which is why fogd supervises it and restarts it on its
          own.
        </li>
        <li>
          To approve the route automatically next time (after a move, or a new LAN address), add an{" "}
          <span className="font-mono text-ink-muted">autoApprovers</span> entry for{" "}
          <span className="font-mono text-ink-muted">{route || "the route"}</span> to your tailnet's access controls.
        </li>
        <li>
          Turning this off leaves the machine in your tailnet — remove it in the admin console if you want it gone
          for good.
        </li>
      </ul>
    </div>
  );
}

// The route is derived from fog's LAN address and needs no attention in the
// normal case, so the override is a quiet line rather than a field — but
// someone reaching the whole LAN through fog needs it, and hiding it would
// mean editing settings by hand.
function RoutesOverride({ st }: { st?: VpnStatus }) {
  const qc = useQueryClient();
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [open, setOpen] = useState(false);

  const save = useMutation({
    mutationFn: () => put("/api/vpn/routes", { routes: value.trim() }),
    onSuccess: () => {
      setError("");
      setSaved(true);
      return qc.invalidateQueries({ queryKey: vpnQuery.queryKey });
    },
    onError: (e) => {
      setSaved(false);
      setError(errorText(e, "Save failed"));
    },
  });

  if (!open) {
    return (
      <Button
        variant="ghost"
        size="sm"
        className="mt-4"
        onClick={() => {
          setValue(st?.route ?? "");
          setOpen(true);
        }}
      >
        Advertise a different route
      </Button>
    );
  }
  return (
    <div className="mt-4 max-w-lg space-y-3">
      <Field
        label="Advertised routes"
        value={value}
        onChange={(e) => {
          setValue(e.target.value);
          setSaved(false);
        }}
        placeholder="192.168.1.200/32"
        aria-invalid={!!error}
        hint={
          error ||
          "Comma-separated CIDRs. Empty restores the default: a host route to fog's own LAN address, which is the one least likely to collide with the network you're sitting on. Advertising the whole LAN reaches everything at home, but collides with any café using the same range — and needs IP forwarding enabled."
        }
      />
      <div className="flex items-center gap-3">
        <Button onClick={() => save.mutate()} disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save"}
        </Button>
        <Button size="sm" onClick={() => setOpen(false)}>Close</Button>
        {saved && <span className="text-sm text-accent">Saved ✓</span>}
      </div>
    </div>
  );
}
