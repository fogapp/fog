// Settings › Network › Domain & HTTPS: buy a domain on Cloudflare, paste a
// token, and Fog does the rest — DNS record, proxy, certificates,
// verification. The page answers "is it set up?" under its title; the
// wizard opens from the button beside it.

import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, type ReachStatus } from "../../api";
import Field from "../../components/Field";
import { Loading, errorText } from "../../components/Async";
import Button from "../../components/Button";
import { StatusLine } from "../../components/Section";
import { SettingsPage } from "./Frame";
import ReachStep, { useReach } from "./Reach";
import { domainStatusKey, reachQuery, settingsQuery } from "../../queries";

interface CheckResult {
  domain: string;
  serverIp: string;
  tokenValid?: boolean;
  tokenError?: string;
  zoneFound?: boolean;
  zoneError?: string;
  zoneActive?: boolean;
  nameServers?: string[];
  conflicts?: string[];
  port80?: { ok: boolean; detail: string };
  port443?: { ok: boolean; detail: string };
}

interface VerifyResult {
  host: string;
  expectedIp: string;
  resolvedIps?: string[];
  resolves: boolean;
  rebindSuspected?: boolean;
  hairpinSuspected?: boolean;
  httpsOk?: boolean;
  httpsError?: string;
  hint?: string;
  fqdnApps?: string[];
}

export interface DomainStatus {
  domain: string;
  tokenSaved: boolean;
  serverIp: string;
  // Where the wildcard is meant to point, and the address that implies —
  // the LAN one, or the public one once the keeper has learned it.
  dnsTarget: "lan" | "wan";
  desiredIp: string;
  proxyRunning: boolean;
  wildcardRecord?: "ok" | "stale" | "missing" | "unknown";
  // The address the stale record actually holds (only set when stale).
  wildcardRecordIp?: string;
  resolvedIps?: string[];
  httpsOk?: boolean;
  fqdnApps?: string[];
}

// Also read by the proxy block, which reports the same running/not-running
// fact — one query, one answer, whichever section you happen to look at.
export function useDomainStatus() {
  return useQuery({
    queryKey: domainStatusKey,
    queryFn: () => get<DomainStatus>("/api/domain/status"),
    staleTime: 30_000,
  });
}

export default function Domain() {
  const qc = useQueryClient();
  const settings = useQuery(settingsQuery);
  const [domain, setDomain] = useState("");
  const [token, setToken] = useState("");
  const [check, setCheck] = useState<CheckResult | null>(null);
  const [dnsResult, setDnsResult] = useState<string>("");
  const [applied, setApplied] = useState(false);
  const [verify, setVerify] = useState<VerifyResult | null>(null);
  const [error, setError] = useState("");
  const [open, setOpen] = useState(false);

  const current = settings.data?.base_domain ?? "";
  const effectiveDomain = domain || current;

  // Initialize from reality: completed steps show as done, and an already
  // working setup opens straight at the success panel.
  const status = useDomainStatus();
  const st = status.data;
  useEffect(() => {
    if (!st) return;
    // "ok" is only reported when the record's content matches this server's
    // current address — a record pointing at an old address shows as stale
    // below instead of a false "already in place".
    if (st.wildcardRecord === "ok" && !dnsResult) {
      setDnsResult(`*.${st.domain} → ${st.desiredIp} (already in place)`);
    }
    if (st.proxyRunning && !applied) {
      setApplied(true);
      setApplyTime(Date.now());
    }
    if (st.httpsOk && !verify) {
      setVerify({
        host: `fog.${st.domain}`, expectedIp: st.desiredIp, resolves: true,
        resolvedIps: st.resolvedIps, httpsOk: true, fqdnApps: st.fqdnApps,
      });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [st]);

  const runCheck = useMutation({
    mutationFn: () => post<CheckResult>("/api/domain/check", { domain: effectiveDomain, token }),
    onSuccess: (r) => {
      setCheck(r);
      setToken("");
      setDnsResult("");
      setApplied(false);
      setVerify(null);
      setError("");
      qc.invalidateQueries({ queryKey: settingsQuery.queryKey });
    },
    onError: (e) => setError(errorText(e, "Check failed")),
  });
  const runDNS = useMutation({
    mutationFn: () => post<{ action: string; record: string }>("/api/domain/dns"),
    onSuccess: (r) => {
      setDnsResult(`${r.record} (${r.action})`);
      qc.invalidateQueries({ queryKey: reachQuery.queryKey });
    },
    onError: (e) => setError(errorText(e, "DNS step failed")),
  });
  const [applyTime, setApplyTime] = useState<number | null>(null);
  const runApply = useMutation({
    mutationFn: () => post("/api/domain/apply"),
    onSuccess: () => {
      setApplied(true);
      setApplyTime(Date.now());
      setVerify(null);
    },
    onError: (e) => setError(errorText(e, "Proxy start failed")),
  });

  // Verification runs itself: poll every 5s from the moment the proxy starts
  // until everything is green — no "wait a bit and click again".
  const verifying = applied && !verify?.httpsOk;
  useQuery({
    queryKey: ["domain-verify", applyTime],
    queryFn: async () => {
      const r = await post<VerifyResult>("/api/domain/verify");
      setVerify(r);
      return r;
    },
    enabled: verifying,
    refetchInterval: 5000,
    retry: false,
  });
  const waitedSeconds = applyTime ? Math.floor((Date.now() - applyTime) / 1000) : 0;
  const redeploy = useMutation({
    mutationFn: (app: string) => post(`/api/apps/${app}/actions`, { action: "deploy" }),
  });

  // Folded, the summary says whether the house is reachable from outside;
  // the step itself keeps the query live while open.
  const reach = useReach(!!current);

  // Both queries feed the "where is this server already at" picture; until
  // they land, the wizard shouldn't imply anything either way.
  const loadingSetup = (!settings.data && !settings.isError) || (!status.data && !status.isError);

  const checksPass =
    check?.tokenValid && check?.zoneFound && check?.zoneActive &&
    (check?.conflicts ?? []).length === 0 && check?.port80?.ok && check?.port443?.ok;
  // A previously completed setup (token + domain saved) doesn't need checks
  // re-run to proceed through the remaining steps.
  const dnsReady = !!checksPass || (!!st?.tokenSaved && !!effectiveDomain);

  return (
    <SettingsPage
      description={
        <>
          One domain gives every app a clean address — <code>manor.your-domain.com</code> — with real
          HTTPS. At home only by default; from anywhere, if you choose, in step 4.
        </>
      }
      status={<DomainSummary loading={loadingSetup} domain={current} httpsOk={verify?.httpsOk || st?.httpsOk} verifying={verifying} reach={reach.data?.status} />}
      actions={
        <Button variant={open || current ? "secondary" : "primary"} onClick={() => setOpen(!open)} aria-expanded={open}>
          {open ? "Close" : current ? "Edit" : "Set up"}
        </Button>
      }
    >
      {open && (
        <div className="max-w-2xl space-y-6">
          <Step n={1} title="Get a domain on Cloudflare">
            <p className="text-sm text-ink-muted">
              Buy one at{" "}
              <a href="https://domains.cloudflare.com" target="_blank" rel="noreferrer" className="text-ink underline">
                Cloudflare Registrar
              </a>{" "}
              (at-cost pricing, and it lands in your Cloudflare account ready to use). Then create an API token at{" "}
              <a href="https://dash.cloudflare.com/profile/api-tokens" target="_blank" rel="noreferrer" className="text-ink underline">
                My Profile → API Tokens
              </a>{" "}
              → Create Token → "Edit zone DNS" template → scope it to your new domain.
            </p>
          </Step>

          <Step n={2} title="Tell Fog about it">
            <div className="space-y-3">
              <Field label="Domain" value={domain} onChange={(e) => setDomain(e.target.value)} placeholder={current || "example.com"} />
              <Field label="Cloudflare API token" type="password" value={token} onChange={(e) => setToken(e.target.value)}
                placeholder={loadingSetup ? "checking for a saved token…" : st?.tokenSaved ? "saved — enter only to replace" : "paste the token"} />
              <Button variant="primary" onClick={() => runCheck.mutate()} disabled={runCheck.isPending || !effectiveDomain}>
                {runCheck.isPending ? "Checking…" : "Run checks"}
              </Button>
              {check && (
                <ul className="space-y-1 text-sm">
                  <CheckLine ok={!!check.tokenValid} label="Cloudflare token" detail={check.tokenError} />
                  {check.tokenValid && (
                    <CheckLine ok={!!check.zoneFound} label={`Domain ${check.domain} on Cloudflare`} detail={check.zoneError} />
                  )}
                  {check.zoneFound && (
                    <CheckLine ok={!!check.zoneActive} label="Domain is active"
                      detail={check.zoneActive ? undefined : `Nameservers still propagating — set them to ${check.nameServers?.join(", ")} at your registrar (automatic for Cloudflare Registrar purchases), then re-run checks.`} />
                  )}
                  {(check.conflicts ?? []).map((c, i) => <CheckLine key={i} ok={false} label="DNS conflict" detail={c} />)}
                  {check.port80 && <CheckLine ok={check.port80.ok} label="Port 80 available" detail={check.port80.ok ? check.port80.detail : `In use: ${check.port80.detail}`} />}
                  {check.port443 && <CheckLine ok={check.port443.ok} label="Port 443 available" detail={check.port443.ok ? check.port443.detail : `In use: ${check.port443.detail}`} />}
                </ul>
              )}
            </div>
          </Step>

          <Step n={3} title="Fog sets everything up">
            <div className="space-y-3">
              <div className="flex flex-wrap items-center gap-3">
                <Button onClick={() => runDNS.mutate()} disabled={!dnsReady || runDNS.isPending}>
                  {runDNS.isPending ? "Creating record…" : "Create DNS record"}
                </Button>
                {dnsResult && <span className="text-sm text-accent">✓ {dnsResult}</span>}
                {!dnsResult && st?.wildcardRecord === "stale" && (
                  <span className="text-sm text-warn">
                    ⚠ The record points at {st.wildcardRecordIp}, but{" "}
                    {st.dnsTarget === "wan" ? "your public address" : "this server"} is now {st.desiredIp} — fog
                    fixes this automatically within a few minutes, or create the record now.
                  </span>
                )}
              </div>
              <div className="flex flex-wrap items-center gap-3">
                <Button onClick={() => runApply.mutate()} disabled={!dnsResult || runApply.isPending}>
                  {runApply.isPending ? "Starting proxy…" : "Start HTTPS proxy"}
                </Button>
                {applied && <span className="text-sm text-accent">✓ Proxy running — certificates issue automatically</span>}
              </div>
              {verifying && (
                <div className="space-y-2 rounded-lg border border-edge bg-veil p-4">
                  <p className="flex items-center gap-2 text-sm text-ink">
                    <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-fog-300 border-t-transparent" />
                    Monitoring DNS propagation and certificate issuance — this usually takes a minute or two.
                    We'll confirm here the moment everything checks out.
                  </p>
                  <ul className="space-y-1 text-sm">
                    <PollLine done={!!verify?.resolves} pending="Waiting for the DNS record to become visible on your network…"
                      doneText={`DNS resolves${verify?.resolvedIps ? ` (${verify.resolvedIps.join(", ")})` : ""}`} />
                    <PollLine done={!!verify?.httpsOk} pending="Waiting for the HTTPS certificate…"
                      doneText="HTTPS answers with a valid certificate" started={!!verify?.resolves} />
                  </ul>
                  {(waitedSeconds > 120 || verify?.hairpinSuspected) && verify?.hint && (
                    <p className="text-xs text-warn">Taking longer than usual: {verify.hint}</p>
                  )}
                  {waitedSeconds > 120 && verify?.httpsError && (
                    <p className="break-words font-mono text-xs text-ink-muted">{verify.httpsError}</p>
                  )}
                </div>
              )}
              {verify?.httpsOk && (
                <div className="space-y-2 rounded-lg border border-accent/30 bg-accent/10 p-4">
                  <p className="text-sm font-semibold text-accent">
                    All set — Fog is now at{" "}
                    <a href={`https://${verify.host}`} className="underline">https://{verify.host}</a>
                  </p>
                  {(verify.fqdnApps ?? []).length > 0 && (
                    <div className="text-sm text-accent">
                      These apps bake the hostname in at deploy time and need one redeploy:{" "}
                      {verify.fqdnApps!.map((a) => (
                        <Button key={a} variant="info" size="sm" className="mr-2" onClick={() => redeploy.mutate(a)}>
                          Redeploy {a}
                        </Button>
                      ))}
                    </div>
                  )}
                </div>
              )}
            </div>
          </Step>

          <Step n={4} title="Reach it from outside (optional)">
            <ReachStep ready={dnsReady} domain={effectiveDomain} />
          </Step>

          {error && <p className="text-sm text-warn">{error}</p>}
        </div>
      )}
    </SettingsPage>
  );
}

// Closed, the wizard is one line: the domain and whether HTTPS actually
// answers on it. The three steps are only interesting while something is
// being set up or changed, so they wait behind the button.
function DomainSummary({ loading, domain, httpsOk, verifying, reach }: {
  loading: boolean; domain: string; httpsOk?: boolean; verifying: boolean; reach?: ReachStatus;
}) {
  if (loading) return <Loading label="Checking your current domain setup…" />;
  if (!domain) return <StatusLine>No domain configured — apps are reachable on the LAN only.</StatusLine>;
  if (verifying) return <StatusLine tone="warn">{domain} — waiting for DNS and the HTTPS certificate…</StatusLine>;
  // A saved domain whose HTTPS never came up is not a working setup, and
  // saying so is the only way the operator knows to reopen the wizard.
  if (!httpsOk) return <StatusLine tone="warn">{domain} — saved, but HTTPS isn't answering yet.</StatusLine>;
  // Outside reach is a claim only once someone has actually got in from
  // outside; until then it's "set to", not "reachable".
  const proven = !!reach && ((!!reach.beacon?.public && !reach.beaconStale) || reach.hairpin === "ok");
  const outside = reach?.target !== "wan" ? " · at home only"
    : proven && reach.hairpin === "failed" ? " · reachable from outside, not from home (no NAT loopback)"
      : proven ? " · reachable from outside"
        : reach.nat === "cgnat" || reach.portMap.error ? " · outside access not working yet"
          : " · set for outside access, not yet confirmed";
  const warn = reach?.target === "wan" && (reach.nat === "cgnat" || reach.hairpin === "failed" || (!!reach.portMap.error && !proven));
  return (
    <StatusLine tone={warn ? "warn" : "ok"}>
      {domain} — HTTPS working{outside}. Fog is at{" "}
      <a href={`https://fog.${domain}`} className="underline">https://fog.{domain}</a>
    </StatusLine>
  );
}

function Step({ n, title, children }: { n: number; title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-hair p-5">
      <h3 className="mb-3 flex items-center gap-2 text-sm font-semibold text-ink">
        <span className="flex h-6 w-6 items-center justify-center rounded-full bg-fog-700 text-xs">{n}</span>
        {title}
      </h3>
      {children}
    </div>
  );
}

function PollLine({ done, pending, doneText, started = true }: { done: boolean; pending: string; doneText: string; started?: boolean }) {
  return (
    <li className="flex items-start gap-2">
      <span className={done ? "text-accent" : "text-ink-muted"}>{done ? "✓" : started ? "◌" : "·"}</span>
      <span className={done ? "text-ink" : "text-ink-muted"}>{done ? doneText : pending}</span>
    </li>
  );
}

function CheckLine({ ok, label, detail }: { ok: boolean; label: string; detail?: string }) {
  return (
    <li className="flex items-start gap-2">
      <span className={ok ? "text-accent" : "text-warn"}>{ok ? "✓" : "✗"}</span>
      <span className="text-ink">
        {label}
        {detail && <span className="block text-xs text-ink-muted">{detail}</span>}
      </span>
    </li>
  );
}
