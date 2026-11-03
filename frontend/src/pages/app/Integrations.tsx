// The Integrations tab: the other apps on the server this app can connect
// to, one entry each, as its fog.yaml's wants name them
// (docs/integrations-contract.md). Connecting sets up everything this app
// needs from that one — credentials minted for it alone, access to the
// app — and redeploys it. What gets set where stays off this page.
// An app a person must approve sends the operator to its page, which sends
// them back here (?connected=, ?declined=, ?error=).

import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, get, post } from "../../api";
import type { IntegrationEntry, IntegrationsList } from "../../api";
import { LoadError, Loading, errorText } from "../../components/Async";
import AppIcon from "../../components/AppIcon";
import Button from "../../components/Button";
import { timeAgo } from "../../lib/format";
import { paths } from "../../lib/paths";
import { catalogQuery } from "../../queries";
import { Group, Pill, Veil } from "./cockpit";

const integrationsKey = (name: string) => ["app", name, "integrations"];

export function IntegrationsTab({ name, onSaved }: { name: string; onSaved: () => void }) {
  const qc = useQueryClient();
  const base = `/api/apps/${encodeURIComponent(name)}/integrations`;
  const q = useQuery({ queryKey: integrationsKey(name), queryFn: () => get<IntegrationsList>(base) });
  const catalog = useQuery(catalogQuery);
  const entryFor = (app: string) => catalog.data?.entries.find((e) => e.id === app);
  const titleOf = (app: string) => entryFor(app)?.title ?? app;
  // What just happened, said once above the lists; and the refusal that
  // stopped a disconnect, with the way past it.
  const [note, setNote] = useState("");
  const [error, setError] = useState<{ text: string; forceProvider?: string } | null>(null);

  // Back from a provider's approval page: say how it went, once.
  const [params, setParams] = useSearchParams();
  useEffect(() => {
    const connected = params.get("connected"), declined = params.get("declined"), failed = params.get("error");
    if (!connected && !declined && !failed) return;
    if (connected) setNote(`Connected to ${titleOf(connected)}. ${redeploySentence(name, params.get("deploying") === "1")}`);
    if (declined) setNote(`The request was declined in ${titleOf(declined)}, so nothing changed.`);
    if (failed) setError({ text: failed });
    setParams({}, { replace: true });
    onSaved();
    // titleOf only refines the wording; the params are what trigger this.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params, setParams, name, onSaved]);

  const settle = async (text: string) => {
    setNote(text);
    setError(null);
    onSaved();
    await qc.invalidateQueries({ queryKey: integrationsKey(name) });
  };

  const connect = useMutation({
    mutationFn: (e: IntegrationEntry) =>
      post<{ deploying?: boolean; consentUrl?: string }>(base, { provider: e.provider, deploy: true }),
    onSuccess: (r, e) => {
      // Approved at the provider, which sends the person back here.
      if (r.consentUrl) return window.location.assign(r.consentUrl);
      return settle(`Connected to ${titleOf(e.provider)}. ${redeploySentence(name, !!r.deploying)}`);
    },
    onError: (err) => { setNote(""); setError({ text: errorText(err, "Couldn't connect") }); },
  });
  const disconnect = useMutation({
    mutationFn: ({ e, force }: { e: IntegrationEntry; force: boolean }) =>
      del<{ deploying: boolean }>(`${base}/${encodeURIComponent(e.provider)}?deploy=1${force ? "&force=1" : ""}`),
    onSuccess: (r, { e }) => settle(e.state === "pending"
      ? `Cancelled the request to ${titleOf(e.provider)}.`
      : `Disconnected from ${titleOf(e.provider)}. ${redeploySentence(name, r.deploying)}`),
    onError: (err, { e }) => { setNote(""); setError({ text: errorText(err, "Couldn't disconnect"), forceProvider: e.provider }); },
  });
  const busy = connect.isPending || disconnect.isPending;

  if (q.isLoading) return <Loading label="Loading integrations…" />;
  if (q.isError || !q.data) return <LoadError label="Couldn't load integrations" error={q.error} onRetry={() => q.refetch()} />;
  const { connected, available } = q.data;
  const appTitle = titleOf(name);

  const card = (e: IntegrationEntry) => {
    const entry = entryFor(e.provider);
    const title = titleOf(e.provider);
    const connecting = connect.isPending && connect.variables?.provider === e.provider;
    return (
      <Veil key={e.provider} className="px-5 py-4">
        <div className="flex flex-wrap items-start gap-4">
          <AppIcon entry={entry} size={44} />
          <div className="min-w-0 flex-1">
            <p className="flex flex-wrap items-center gap-2 text-cell text-ink">
              {title}
              {e.state === "pending" ? <Pill>waiting for approval</Pill>
                : e.state === "connected" && (e.problem ? <Pill tone="warn">needs attention</Pill> : <Pill tone="ok">connected</Pill>)}
            </p>
            {e.capabilities.length > 0 && (
              <ul className="mt-2 list-disc space-y-0.5 pl-5 text-hint text-ink-muted marker:text-ink-faint">
                {e.capabilities.map((c) => <li key={c}>{c}</li>)}
              </ul>
            )}
            {e.state === "connected" && !e.problem && (
              <p className="mt-2 text-hint text-ink-muted">
                {e.lastActivityAt ? `${title} last heard from ${appTitle} ${timeAgo(e.lastActivityAt)}.`
                  : e.connectedAt ? `${title} hasn't heard from ${appTitle} yet.` : null}
                {e.connectedAt && ` Connected ${timeAgo(e.connectedAt)}${e.connectedBy ? ` by ${e.connectedBy}` : ""}.`}
              </p>
            )}
            {e.incomplete && !e.problem && (
              <p className="mt-2 text-hint text-ink-muted">
                {appTitle} can do more with {title} since this was connected. Update to add it.
              </p>
            )}
            {e.replaces && (e.state === "available" || e.incomplete) && (
              <p className="mt-2 text-hint text-warn">Connecting replaces settings entered by hand in Config.</p>
            )}
            {e.problem && (
              <p className={`mt-2 text-hint ${e.state === "pending" ? "text-ink-muted" : "text-warn"}`}>
                {e.problem}
                {!e.installed && e.state === "available" && entry && (
                  <> · <Link to={paths.storeApp(e.provider)} className="underline decoration-dotted underline-offset-2 hover:text-ink">See it in the store</Link></>
                )}
              </p>
            )}
          </div>
          <div className="flex shrink-0 gap-2">
            {e.incomplete && (
              <Button size="sm" variant="primary" disabled={busy || !!e.problem} onClick={() => connect.mutate(e)}>
                {connecting ? "Updating…" : e.consent ? "Update…" : "Update"}
              </Button>
            )}
            {e.state === "available" ? (
              <Button size="sm" variant="primary" disabled={busy || !!e.problem} onClick={() => connect.mutate(e)}>
                {connecting ? "Connecting…" : e.consent ? "Connect…" : "Connect"}
              </Button>
            ) : e.state === "pending" ? (
              <Button size="sm" disabled={busy} onClick={() => disconnect.mutate({ e, force: false })}>Cancel</Button>
            ) : (
              <Button size="sm" variant="danger" disabled={busy} onClick={() => disconnect.mutate({ e, force: false })}>
                Disconnect
              </Button>
            )}
          </div>
        </div>
      </Veil>
    );
  };

  return (
    <div>
      {(note || error) && (
        <Veil className="mb-6 px-5 py-3">
          {note && <p className="text-cell text-ink">{note}</p>}
          {error && (
            <div className="flex flex-wrap items-center gap-3">
              <p className="text-cell text-warn">{error.text}</p>
              {error.forceProvider !== undefined && (
                <Button size="sm" variant="danger" disabled={busy}
                  onClick={() => { const e = connected.find((x) => x.provider === error.forceProvider); if (e) disconnect.mutate({ e, force: true }); }}>
                  Disconnect anyway
                </Button>
              )}
            </div>
          )}
        </Veil>
      )}

      {connected.length === 0 && available.length === 0 ? (
        <Group title="Integrations" first>
          <p className="text-hint text-ink-muted">{appTitle} doesn't connect to any other apps.</p>
        </Group>
      ) : (
        <>
          {connected.length > 0 && (
            <Group title="Connected" first>
              <div className="space-y-3">{connected.map(card)}</div>
            </Group>
          )}
          {available.length > 0 && (
            <Group title="Available" first={connected.length === 0}>
              <div className="space-y-3">{available.map(card)}</div>
              <p className="mt-4 text-hint text-ink-muted">
                Connecting sets up everything {appTitle} needs and redeploys it.
                An app marked “Connect…” asks for approval in that app first, then sends you back here.
              </p>
            </Group>
          )}
        </>
      )}
    </div>
  );
}

function redeploySentence(name: string, deploying: boolean): string {
  return deploying ? `${name} is redeploying to pick it up.` : `${name} picks it up when it next starts.`;
}
