// The address group in the standing column: where the app answers, and the
// one edit that moves it. The subdomain is the only half of a hostname an
// operator picks — the base domain is Fog's — so the editor is the same
// label-plus-suffix field the installer uses. Saving writes a spec revision
// whose routes carry the new label; the proxy follows the revision at once,
// and the app itself is redeployed only when its env quotes {{fqdn}}, since
// that is the one way the old address can be baked into the container.

import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { put, type AppDetail as AppDetailT, type AppSpec, type AppStatus } from "../../api";
import AddressField from "../../components/AddressField";
import { errorText } from "../../components/Async";
import Button from "../../components/Button";
import { reservedSubdomain, routeHost, validateSubdomain } from "../../lib/hostname";
import { useApps } from "../../lib/useApps";
import { Group, SlotLink } from "./cockpit";
import { settingsQuery } from "../../queries";

export function AddressGroup({ name, detail, status, gap, onSaved }: {
  name: string; detail: AppDetailT; status?: AppStatus; gap: string; onSaved: () => Promise<unknown>;
}) {
  const [editing, setEditing] = useState(false);
  const settings = useQuery(settingsQuery);
  const domain = settings.data?.base_domain ?? "";
  const routes = detail.spec.routes ?? [];
  // The address the app is known by: the first route's, the same one the
  // subheader's Open button and {{fqdn}} resolve to.
  const primary = routes.length > 0 ? routeHost(name, routes[0].host, domain || undefined) : null;
  const others = (status?.hosts ?? []).filter((h) => h !== primary);

  if (routes.length === 0) {
    return (
      <Group title="Address" gap={gap}>
        <p className="text-hint text-ink-muted">No web address — this app has no routes.</p>
      </Group>
    );
  }

  return (
    <Group title="Address" gap={gap} right={!editing && domain ? <SlotLink onClick={() => setEditing(true)}>Change</SlotLink> : undefined}>
      {editing ? (
        <AddressEditor name={name} spec={detail.spec} domain={domain} current={primary}
          onClose={() => setEditing(false)} onSaved={onSaved} />
      ) : (
        <div>
          {primary ? (
            <a href={`https://${primary}`} target="_blank" rel="noreferrer" className="font-mono text-cell text-ink hover:text-accent">
              {primary}
            </a>
          ) : (
            <div className="text-hint text-ink-muted">
              {settings.data ? "No base domain yet — set one in Settings → Network → Domain & HTTPS to give this app an address." : "Loading your domain…"}
            </div>
          )}
          {others.map((h) => (
            <div key={h} className="mt-1 font-mono text-hint text-ink-muted">{h}</div>
          ))}
        </div>
      )}
    </Group>
  );
}

// labelOf is the subdomain half of a route's address as the spec stores it:
// the app's name when the route is hostless, the host itself when it is a
// bare label, and null when it is pinned to a dotted hostname of its own.
function labelOf(name: string, host: string | undefined): string | null {
  if (!host) return name;
  return host.includes(".") ? null : host;
}

function AddressEditor({ name, spec, domain, current, onClose, onSaved }: {
  name: string; spec: AppSpec; domain: string; current: string | null; onClose: () => void; onSaved: () => Promise<unknown>;
}) {
  const from = labelOf(name, spec.routes?.[0]?.host);
  const [value, setValue] = useState(from ?? "");
  const label = value.trim().toLowerCase();
  const effective = label || name;
  const unchanged = effective === from;

  // Hosts other apps already answer on, so a clash is caught here rather
  // than becoming an unreachable app. Undefined until the list lands.
  const apps = useApps();
  const taken = apps.data
    ? new Set(apps.data.filter((a) => a.name !== name).flatMap((a) => a.hosts ?? []).concat(`fog.${domain}`))
    : undefined;
  const clash = !unchanged && taken?.has(`${effective}.${domain}`)
    ? `${effective}.${domain} is already served by another app — the proxy can only route a hostname to one of them.`
    : null;
  const error =
    validateSubdomain(value) ??
    (label && reservedSubdomain(label) ? `“${label}” is reserved for Fog itself — pick another address.` : null) ??
    clash;

  // Only a container that was told its own address needs to be rebuilt to
  // learn the new one; for everything else the proxy change is the whole move.
  const redeploys = Object.values(spec.env ?? {}).some((v) => v.includes("{{fqdn}}"));

  const save = useMutation({
    mutationFn: async () => {
      const next = moveRoutes(name, spec, effective);
      await put(`/api/apps/${name}`, {
        spec: next,
        secrets: {},
        comment: `Address changed to ${effective}.${domain}`,
        deploy: redeploys,
      });
      await onSaved();
    },
    onSuccess: onClose,
  });

  return (
    <form
      className="space-y-3"
      onSubmit={(e) => { e.preventDefault(); if (!error && !unchanged && !save.isPending) save.mutate(); }}
    >
      <AddressField value={value} onChange={setValue} placeholder={name} baseDomain={domain} loaded
        error={error} label={null} autoFocus />
      {!error && !unchanged && (
        <p className="text-xs text-ink-muted">
          {current ? `${current} stops answering. ` : ""}
          {redeploys
            ? "The app restarts so its own settings learn the new address."
            : "Takes effect as soon as it's saved."}
        </p>
      )}
      {save.isError && <p className="text-xs text-warn">{errorText(save.error, "Couldn't change the address")}</p>}
      <div className="flex flex-wrap gap-2">
        <Button size="sm" variant="primary" type="submit" disabled={!!error || unchanged || save.isPending || apps.isPending}>
          {save.isPending ? "Saving…" : redeploys ? "Save and restart" : "Save"}
        </Button>
        <Button size="sm" variant="ghost" type="button" onClick={onClose} disabled={save.isPending}>Cancel</Button>
      </div>
    </form>
  );
}

// moveRoutes returns the spec with the primary route, and every other route
// sharing its label, pointed at the new label. Routes pinned to a dotted
// hostname or to some other label are someone else's business and stay put.
// A label that is just the app's name is stored as the default (no host) so
// the spec keeps tracking the name, the way a fresh install does.
function moveRoutes(name: string, spec: AppSpec, label: string): AppSpec {
  const from = labelOf(name, spec.routes?.[0]?.host);
  const host = label === name ? undefined : label;
  const routes = (spec.routes ?? []).map((r, i) => {
    if (i !== 0 && (from === null || labelOf(name, r.host) !== from)) return r;
    const next = { ...r };
    delete next.host;
    return host ? { ...next, host } : next;
  });
  return { ...spec, routes };
}
