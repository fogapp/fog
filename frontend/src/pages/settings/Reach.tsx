// Step 4 of the domain wizard: where DNS points. Off, *.<domain> resolves
// to this server's LAN address — names work at home, nothing is exposed.
// On, it resolves to the address the internet sees the house at, so the
// same links work from a phone on the road; fog then keeps that address
// current (the ISP changes it) and asks the router to send port 443 here.
// Every line below is a fact the keeper established, never an assumption:
// the public address it learned, what the router said, and whether anyone
// has actually reached the house from outside.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { post, put, type ReachResponse, type ReachStatus } from "../../api";
import Button from "../../components/Button";
import CopyButton from "../../components/CopyButton";
import SettingsLink from "../../components/SettingsLink";
import Toggle from "../../components/Toggle";
import { Loading, errorText } from "../../components/Async";
import { timeAgo, timeUntil } from "../../lib/format";
import { useState } from "react";
import { domainStatusKey, reachQuery } from "../../queries";

export function useReach(enabled: boolean) {
  return useQuery({
    ...reachQuery,
    enabled,
    // The keeper checks every five minutes on its own; polling here only
    // has to catch the beacon hit and a Refresh that just ran.
    refetchInterval: 15_000,
  });
}

export default function ReachStep({ ready, domain }: { ready: boolean; domain: string }) {
  const qc = useQueryClient();
  const reach = useReach(true);
  const st = reach.data?.status;
  const [error, setError] = useState("");

  const refreshAll = () => {
    qc.invalidateQueries({ queryKey: domainStatusKey });
    return qc.invalidateQueries({ queryKey: reachQuery.queryKey });
  };
  const update = useMutation({
    mutationFn: (body: { target: "lan" | "wan"; portMap?: boolean }) => put<ReachResponse>("/api/domain/reach", body),
    onSuccess: (r) => {
      setError("");
      qc.setQueryData(reachQuery.queryKey, r);
      return refreshAll();
    },
    onError: (e) => setError(errorText(e, "Couldn't change where DNS points")),
  });
  const refresh = useMutation({
    mutationFn: () => post<ReachResponse>("/api/domain/refresh"),
    onSuccess: (r) => {
      setError("");
      qc.setQueryData(reachQuery.queryKey, r);
      return refreshAll();
    },
    onError: (e) => setError(errorText(e, "Refresh failed")),
  });

  const busy = update.isPending || refresh.isPending;
  const wan = st?.target === "wan";

  if (!st && !reach.isError) return <Loading label="Checking where DNS points…" />;
  if (!st) return <p className="text-sm text-ink-muted">Reach status unavailable.</p>;

  return (
    <div className="space-y-4">
      <Toggle
        checked={wan}
        disabled={!ready || busy}
        onChange={(on) => update.mutate({ target: on ? "wan" : "lan" })}
        label="Point DNS at my home's public address, so apps work away from home"
        className="text-sm text-ink"
      />
      <p className="text-xs text-ink-muted">
        {wan ? (
          <>
            <span className="font-mono text-ink-muted">*.{domain}</span> resolves to the address the internet sees your
            house at. Fog re-checks it every few minutes and rewrites the record when your ISP changes it, and asks the
            router to send port 443 here. Anything without fog's login is then open to the internet — keep the login on.
          </>
        ) : (
          <>
            <span className="font-mono text-ink-muted">*.{domain}</span> resolves to this server's LAN address
            {st.lanIp ? <> (<span className="font-mono">{st.lanIp}</span>)</> : null}. Names work at home; nothing is
            reachable from outside. {!ready && "Finish step 3 first."}
          </>
        )}
      </p>

      {wan && <WanReport st={st} beaconUrl={reach.data?.beaconUrl} busy={busy}
        onPortMap={(on) => update.mutate({ target: "wan", portMap: on })} />}

      <div className="flex flex-wrap items-center gap-3">
        <Button onClick={() => refresh.mutate()} disabled={!ready || busy}>
          {refresh.isPending ? "Refreshing…" : "Refresh DNS now"}
        </Button>
        <span className="text-xs text-ink-muted">
          {st.checkedAt ? <>checked {timeAgo(st.checkedAt)}</> : "not checked yet"}
          {st.confirmedAt && <> · record confirmed {timeAgo(st.confirmedAt)}</>}
          {st.failing && st.lastError && <span className="text-warn"> · failing: {st.lastError}</span>}
        </span>
      </div>
      {error && <p className="text-sm text-warn">{error}</p>}
    </div>
  );
}

// The outside-reach report, one line per link in the chain: public address,
// DNS record, router, and proof from outside.
function WanReport({ st, beaconUrl, busy, onPortMap }: {
  st: ReachStatus; beaconUrl?: string; busy: boolean; onPortMap: (on: boolean) => void;
}) {
  const pm = st.portMap;
  const recordOk = !!st.recordIp && st.recordIp === st.desiredIp && !st.lastError;
  const opened443 = (pm.opened ?? []).includes(443);
  const cgnat = st.nat === "cgnat";
  // Proof from outside counts only for the current public address; a
  // passing self-test from inside is proof too (loopback applies the
  // forward rules).
  const confirmed = (!!st.beacon?.public && !st.beaconStale) || st.hairpin === "ok";

  return (
    <div className="space-y-3 rounded-lg border border-edge bg-veil p-4">
      <ul className="space-y-1.5 text-sm">
        <Line state={st.publicIp ? "ok" : "bad"} text={st.publicIp ? <>Public address <span className="font-mono">{st.publicIp}</span></> : "Public address unknown"}
          detail={st.publicIpError} />
        <Line state={recordOk ? "ok" : st.desiredIp ? "warn" : "idle"}
          text={recordOk
            ? <>DNS record <span className="font-mono">*.{st.domain}</span> → <span className="font-mono">{st.recordIp}</span></>
            : st.recordIp
              ? <>DNS record still holds <span className="font-mono">{st.recordIp}</span>, should be <span className="font-mono">{st.desiredIp}</span></>
              : "DNS record not written yet"}
          detail={st.lastError} />
        {cgnat ? (
          <Line state="bad" text="Your ISP puts this house behind carrier-grade NAT"
            detail={<>The router's own internet address ({pm.externalIp}) isn't the one the world sees ({st.publicIp}),
              so no port forward can reach here. Use <SettingsLink to="network/tunnel" /> instead — it needs no open ports.</>} />
        ) : !pm.enabled ? (
          <Line state={confirmed ? "ok" : "idle"} text="Automatic port opening is off"
            detail={<>Forward TCP 443 (and 80) on the router to <span className="font-mono">{st.lanIp}</span> yourself
              {st.gateway && <> — its admin page is usually at <a className="underline" href={`http://${st.gateway}`} target="_blank" rel="noreferrer">http://{st.gateway}</a></>}.</>} />
        ) : opened443 ? (
          <Line state="ok"
            text={<>Router opened port{(pm.opened ?? []).length > 1 ? "s" : ""} {(pm.opened ?? []).join(" and ")} automatically ({pm.method === "upnp" ? "UPnP" : "NAT-PMP"})</>}
            detail={<>{pm.permanent ? "Kept until fog releases it." : `Lease renews on its own${pm.expiresAt ? `; the current one runs out ${timeUntil(pm.expiresAt)}` : ""}.`}
              {pm.failed && Object.keys(pm.failed).length > 0 && <> Port {Object.keys(pm.failed).join(", ")} was refused: {Object.values(pm.failed).join("; ")} — only 443 is needed.</>}</>} />
        ) : (
          <Line state={confirmed ? "ok" : "bad"} text={confirmed ? "Router didn't open ports automatically, but the house is reachable" : "Router didn't open the ports automatically"}
            detail={<>
              {pm.error && <span className="block font-mono text-ink-muted">{pm.error}</span>}
              Forward TCP 443 (and 80) to <span className="font-mono">{st.lanIp}</span> in the router's settings
              {st.gateway && <> — usually at <a className="underline" href={`http://${st.gateway}`} target="_blank" rel="noreferrer">http://{st.gateway}</a></>}.
              Look for "Port forwarding", "Virtual server", or "NAT" and point external port 443 at this server's port 443.
              Or turn on UPnP in the router and press Refresh.
            </>} />
        )}
        <Line state={st.hairpin === "ok" ? "ok" : st.hairpin === "failed" ? (confirmed ? "bad" : "warn") : "idle"}
          text={st.hairpin === "ok"
            ? "Self-test from inside passed — the router forwards 443 and loops it back for devices at home"
            : st.hairpin === "failed"
              ? st.hairpinAnswerer
                ? "Self-test from inside reached something else on port 443 — the router's own admin page, most likely"
                : confirmed
                  ? "Reachable from outside, but not from inside: this router has no NAT loopback"
                  : "Self-test from inside failed — a missing forward, or a router without NAT loopback"
              : "Self-test from inside not run yet"}
          detail={st.hairpin === "failed" && (
            <>
              {st.hairpinError && <span className="block font-mono text-ink-muted">{st.hairpinError}</span>}
              {st.hairpinAnswerer ? (
                <>The certificate at <span className="font-mono">{st.publicIp}:443</span> is for <span className="font-mono">{st.hairpinAnswerer}</span>, not fog.
                  The router loops traffic back fine; port 443 is just taken, which is also why it refused to open the port automatically.
                  Add a manual forward of external port 443 to <span className="font-mono">{st.lanIp}</span> — it takes priority over the router's own page, which stays reachable at its LAN address.</>
              ) : confirmed && <>Devices at home can't reach the names until the router's NAT loopback (hairpin) is enabled, or{" "}
                <span className="font-mono">{st.domain}</span> gets a local DNS override to <span className="font-mono">{st.lanIp}</span> in the router or Pi-hole.</>}
            </>
          )} />
        <Line state={st.beacon?.public && !st.beaconStale ? "ok" : st.beacon ? "warn" : "idle"}
          text={st.beacon?.public && !st.beaconStale
            ? <>Reached from outside {timeAgo(st.beacon.at)} (from <span className="font-mono">{st.beacon.ip}</span>)</>
            : st.beacon?.public && st.beaconStale
              ? <>Confirmed from outside {timeAgo(st.beacon.at)}, but under a previous public address ({st.beacon.publicIp}) — confirm again</>
              : st.beacon
                ? <>Last visit came from inside the house ({timeAgo(st.beacon.at)}) — that doesn't prove outside access yet</>
                : "Not yet confirmed from outside"}
          detail={!(st.beacon?.public && !st.beaconStale) && beaconUrl && (
            <span className="flex flex-wrap items-center gap-2">
              <span>Open this link on your phone with Wi-Fi off:</span>
              <span className="break-all font-mono text-ink-muted">{beaconUrl}</span>
              <CopyButton text={beaconUrl} />
            </span>
          )} />
      </ul>
      {!cgnat && (
        <Toggle checked={pm.enabled} disabled={busy} onChange={onPortMap}
          label="Ask the router to open the ports automatically (UPnP / NAT-PMP)" />
      )}
    </div>
  );
}

function Line({ state, text, detail }: { state: "ok" | "warn" | "bad" | "idle"; text: React.ReactNode; detail?: React.ReactNode }) {
  const mark = state === "ok" ? "✓" : state === "bad" ? "✗" : state === "warn" ? "⚠" : "◌";
  const tone = state === "ok" ? "text-accent" : state === "idle" ? "text-ink-muted" : "text-warn";
  return (
    <li className="flex items-start gap-2">
      <span className={tone}>{mark}</span>
      <span className="text-ink">
        {text}
        {detail && <span className="mt-0.5 block text-xs text-ink-muted">{detail}</span>}
      </span>
    </li>
  );
}
