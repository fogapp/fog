// The Config tab: what this fog is to a peer — its name, its key's
// fingerprint, the address peers dial, and whether they can. Whether a
// peer's breakage wakes your phone is a notification preference, and lives
// with the others in Settings; this tab points there.

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError, post, put, type FederationInfo } from "../../api";
import { errorText } from "../../components/Async";
import Button from "../../components/Button";
import CopyButton from "../../components/CopyButton";
import Field from "../../components/Field";
import { versionLabel } from "../../lib/format";
import { Dot, Group, Veil } from "../app/cockpit";
import SettingsLink from "../../components/SettingsLink";
import { federationQuery } from "../../queries";

export function ConfigTab({ info }: { info: FederationInfo }) {
  return (
    <div className="max-w-3xl">
      <Group title="This fog" first right={info.identity.fogVersion && <span className="text-hint text-ink-muted">fog {versionLabel(info.identity.fogVersion)}</span>}>
        <Identity info={info} />
      </Group>

      {!info.identity.keyBound && (
        <Group title="Renew identity">
          <Renew />
        </Group>
      )}

      <Group title="Tell me when">
        <Veil className="px-[18px] py-4 text-hint text-ink-muted">
          Whether a peer's breakage pushes to your phone is set with the other notifications, under{" "}
          <SettingsLink to="preferences/notifications" anchor="when" />.
        </Veil>
      </Group>
    </div>
  );
}

function Identity({ info }: { info: FederationInfo }) {
  const qc = useQueryClient();
  const id = info.identity;
  const r = info.reachability;
  const [url, setUrl] = useState(id.url ?? "");
  const [error, setError] = useState("");
  const saveURL = useMutation({
    mutationFn: (u: string) => put("/api/federation/identity", { url: u }),
    onSuccess: () => {
      setError("");
      return qc.invalidateQueries({ queryKey: federationQuery.queryKey });
    },
    onError: (e) => setError(errorText(e, "Couldn't save the address")),
  });
  const dirty = url.trim() !== (id.url ?? "");

  return (
    <Veil className="space-y-4 px-[18px] py-4 text-sm">
      <div className="flex items-baseline gap-3">
        <span className="text-[19px] font-medium text-ink">{id.name}</span>
        <span className="text-hint text-ink-muted">how peers see this fog</span>
      </div>
      <Line label="Fingerprint" copy={id.fingerprintFull}>
        <span className="break-all font-mono text-cell text-ink">{id.fingerprintFull ?? "—"}</span>
      </Line>
      <Line label="Instance" copy={id.instanceId}>
        <span className="break-all font-mono text-cell text-ink-muted">{id.instanceId}</span>
      </Line>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          saveURL.mutate(url.trim());
        }}
      >
        <Field
          label="Address peers dial"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="https://fog.your.domain"
          hint={error || (id.url ? "Heartbeats carry it, so peers learn a change without re-pairing." : "No address yet — peers can't dial this fog, but a peer this fog dials keeps both sides fresh.")}
          aria-invalid={!!error}
        />
        {dirty && (
          <div className="mt-2 flex justify-end gap-2">
            <Button size="sm" variant="ghost" type="button" onClick={() => setUrl(id.url ?? "")}>Cancel</Button>
            <Button size="sm" variant="primary" type="submit" disabled={saveURL.isPending}>{saveURL.isPending ? "Saving…" : "Save"}</Button>
          </div>
        )}
      </form>

      {r.reachable ? (
        <p className="inline-flex items-center gap-2 text-hint text-accent">
          <Dot tone="ok" />
          Reachable — peers can dial you{r.via === "domain" ? " at your domain" : ""}.{r.tunnel.running && <span className="text-ink-muted"> · tunnel up</span>}
        </p>
      ) : (
        <p className="rounded-md border border-warn/30 bg-warn/10 px-2.5 py-1.5 text-hint leading-relaxed text-warn">{r.advice}</p>
      )}
    </Veil>
  );
}

// Renew: this fog's identity predates instance IDs derived from keys, so it
// pairs with no new household until it gets a key-bound one. Renewing is
// never quiet: when anything knows this fog by its old identity, the first
// press shows what would stop working and asks again.
function Renew() {
  const qc = useQueryClient();
  const [warning, setWarning] = useState("");
  const [error, setError] = useState("");
  const renew = useMutation({
    mutationFn: (breakPairings: boolean) => post("/api/federation/identity/renew", { breakPairings }),
    onSuccess: () => {
      setWarning("");
      setError("");
      return qc.invalidateQueries({ queryKey: federationQuery.queryKey });
    },
    onError: (e) => {
      if (e instanceof ApiError && e.status === 409 && (e.body as { households?: unknown })?.households) {
        setWarning(e.message);
        return;
      }
      setError(errorText(e, "Couldn't renew the identity"));
    },
  });
  return (
    <Veil className="space-y-3 px-[18px] py-4 text-sm">
      <p className="rounded-md border border-warn/30 bg-warn/10 px-2.5 py-1.5 text-hint leading-relaxed text-warn">
        This fog's identity predates key-bound instance IDs, so it can't pair with a new household until it is renewed. Households already paired keep working.
      </p>
      {warning && <p className="text-hint leading-relaxed text-ink">{warning}</p>}
      {error && <p className="text-hint text-warn">{error}</p>}
      <div className="flex justify-end gap-2">
        {warning && <Button size="sm" variant="ghost" type="button" onClick={() => setWarning("")}>Cancel</Button>}
        <Button size="sm" variant={warning ? "danger" : "primary"} type="button" disabled={renew.isPending} onClick={() => renew.mutate(!!warning)}>
          {renew.isPending ? "Renewing…" : warning ? "Renew anyway" : "Renew identity"}
        </Button>
      </div>
    </Veil>
  );
}

function Line({ label, copy, children }: { label: string; copy?: string; children: React.ReactNode }) {
  return (
    <div className="grid items-center gap-3.5" style={{ gridTemplateColumns: "110px minmax(0,1fr) auto" }}>
      <span className="text-hint text-ink-muted">{label}</span>
      <span className="min-w-0">{children}</span>
      <span>{copy && <CopyButton text={copy} />}</span>
    </div>
  );
}
