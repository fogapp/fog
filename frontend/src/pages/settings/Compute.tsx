// Machines that lend fog their hardware (docs/compute-contract.md).
// A machine is paired once, with a code shown here and a command run on
// it; from then on its hardware joins a pool, and every app that can use
// one sees the pool as a single transcoder fog provides — never the
// machine behind it. Swapping a machine is a pairing here, not a change
// in any app.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, patch, post, put, type ComputeMachine, type ComputePool, type ComputeUsage, type Slots } from "../../api";
import { LoadError, Loading, errorText } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import CopyButton from "../../components/CopyButton";
import { Select } from "../../components/Field";
import Toggle from "../../components/Toggle";
import { Section, StatusLine } from "../../components/Section";
import { RowItem, RowList } from "../../components/SettingRow";
import { computeQuery } from "../../queries";
import ComputeActivity from "./ComputeActivity";
import { SettingsPage } from "./Frame";

interface PairingCode {
  code: string;
  command: string;
  expiresAt: string;
}

export default function Compute() {
  const qc = useQueryClient();
  const overview = useQuery(computeQuery);
  const [pairing, setPairing] = useState<PairingCode | null>(null);
  const [error, setError] = useState("");
  const [ask, dialog] = useConfirm();

  const newCode = useMutation({
    mutationFn: () => post<PairingCode>("/api/compute/codes"),
    onSuccess: (c) => {
      setError("");
      setPairing(c);
    },
    onError: (e) => setError(errorText(e, "Couldn't make a pairing code")),
  });
  const refresh = () => qc.invalidateQueries({ queryKey: computeQuery.queryKey });
  const grant = useMutation({
    mutationFn: (g: { app: string; pool: string; on: boolean }) => put("/api/compute/grants", g),
    onSuccess: refresh,
    onError: (e) => setError(errorText(e, "Couldn't change that")),
  });
  const update = useMutation({
    mutationFn: ({ id, ...changes }: { id: string; paused?: boolean; budget?: Slots; clearBudget?: boolean }) =>
      patch(`/api/compute/nodes/${id}`, changes),
    onSuccess: refresh,
    onError: (e) => setError(errorText(e, "Couldn't change the machine")),
  });
  const updateNow = useMutation({
    mutationFn: (id: string) => post(`/api/compute/nodes/${id}/update`),
    onSuccess: refresh,
    onError: (e) => setError(errorText(e, "Couldn't ask it to update")),
  });
  const remove = useMutation({
    mutationFn: (id: string) => del(`/api/compute/nodes/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: computeQuery.queryKey }),
    onError: (e) => setError(errorText(e, "Couldn't remove the machine")),
  });

  const machines = overview.data?.machines;
  const pools = overview.data?.pools;
  const online = machines?.filter((m) => m.online).length ?? 0;

  return (
    <SettingsPage
      description={
        <>
          Other machines in the house can lend fog their hardware — a Mac's media engines for transcoding, its GPU for
          AI models. Apps never see the machine: each pool of alike machines is one transcoder fog provides, so a
          machine can be swapped here without changing anything in an app.
        </>
      }
      status={
        !machines ? undefined : machines.length === 0 ? (
          <StatusLine>No machines yet — everything runs on this server.</StatusLine>
        ) : (
          <StatusLine tone={online > 0 ? "ok" : "warn"}>
            {online} of {machines.length} machine{machines.length === 1 ? "" : "s"} online.
          </StatusLine>
        )
      }
      actions={
        <Button variant="primary" disabled={newCode.isPending} onClick={() => newCode.mutate()}>
          {newCode.isPending ? "Making a code…" : "Add a machine"}
        </Button>
      }
    >
      {pairing && (
        <div className="space-y-2 rounded-lg border border-accent/30 bg-accent/10 p-4 text-sm">
          <p className="text-accent">
            On the machine, run this in a terminal. The code <span className="font-mono">{pairing.code}</span> pairs one
            machine and lasts until {new Date(pairing.expiresAt).toLocaleTimeString()}.
          </p>
          <div className="flex items-center gap-2">
            <code className="min-w-0 flex-1 break-all rounded-md bg-fog-950 p-2 font-mono text-xs text-ink">
              {pairing.command}
            </code>
            <CopyButton text={pairing.command} />
          </div>
          <p className="text-xs text-ink-muted">
            It fetches fog-node from this server, checks it, installs ffmpeg if it's missing (with Homebrew on a Mac),
            pairs, and starts fog-node at login. The machine appears below within a few seconds.
          </p>
          <Button variant="ghost" size="sm" onClick={() => setPairing(null)}>Dismiss</Button>
        </div>
      )}

      <Section
        id="pools"
        title="What apps see"
        description="One transcoder per kind of hardware, with the room of every machine in it. A viewer's conversion goes first, and stops batch work when a machine is full; fog · Inference routes each request to a machine with the model it names. Switch a pool off for an app to keep it to this server."
      >
        {!pools && !overview.isError && <Loading label="Loading…" />}
        {overview.isError && <LoadError label="Couldn't load compute." error={overview.error} onRetry={() => overview.refetch()} />}
        {pools && pools.length === 0 && <p className="text-sm text-ink-muted">No pools yet — pair a machine with a hardware encoder.</p>}
        {pools && pools.length > 0 && (
          <RowList>
            {pools.map((p) => (
              <PoolRow
                key={p.id}
                pool={p}
                machines={machines ?? []}
                apps={(overview.data?.apps ?? []).filter((a) => a.pool === p.id)}
                onGrant={(app, on) => grant.mutate({ app, pool: p.id, on })}
                busy={grant.isPending}
              />
            ))}
          </RowList>
        )}
      </Section>

      <Section id="machines" title="Machines">
        {machines && machines.length === 0 && <p className="text-sm text-ink-muted">None paired.</p>}
        {machines && machines.length > 0 && (
          <RowList>
            {machines.map((m) => (
              <RowItem
                key={m.id}
                title={
                  <span className="flex items-center gap-2">
                    <span className="truncate">{m.name}</span>
                    {m.connectedAt ? (
                      <span className={m.online ? "text-[11px] text-ok" : "text-[11px] text-ink-muted"}>
                        {m.online ? "online" : "offline"}
                      </span>
                    ) : (
                      <span className="text-[11px] text-warn" title="Its service hasn't reached fog since pairing; the Inbox says how to find out why">
                        never connected
                      </span>
                    )}
                    {m.behind && (
                      <span
                        className={m.behind.stuck ? "whitespace-nowrap text-[11px] text-warn" : "whitespace-nowrap text-[11px] text-ink-muted"}
                        title={m.behind.stuck
                          ? `It runs fog-node ${m.inventory.version} and hasn't taken fog's ${m.behind.want}; the Inbox says why`
                          : `fog is updating it from fog-node ${m.inventory.version} to ${m.behind.want}`}
                      >
                        {m.behind.stuck ? "behind" : "updating"}
                      </span>
                    )}
                  </span>
                }
                meta={<MachineFacts machine={m} />}
                actions={
                  <span className="flex flex-wrap items-center gap-3">
                    <Toggle
                      label="Lend it"
                      checked={!m.paused}
                      disabled={update.isPending}
                      onChange={(on) => update.mutate({ id: m.id, paused: !on })}
                    />
                    {m.behind && m.online && (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={updateNow.isPending && updateNow.variables === m.id}
                        title={`Have it update to fog-node ${m.behind.want} now`}
                        onClick={() => updateNow.mutate(m.id)}
                      >
                        Update now
                      </Button>
                    )}
                    {m.pool && <BudgetPicker machine={m} disabled={update.isPending} onChange={(budget) => update.mutate(budget ? { id: m.id, budget } : { id: m.id, clearBudget: true })} />}
                    <Button
                      size="sm"
                      variant="danger"
                      disabled={remove.isPending && remove.variables === m.id}
                      onClick={async () => {
                        if (await ask({
                          title: `Remove ${m.name}?`,
                          body: "Its credential stops working at once, and whatever it was doing for apps is handed back to them. Pair it again to lend it once more.",
                          action: "Remove",
                          danger: true,
                        })) remove.mutate(m.id);
                      }}
                    >
                      Remove
                    </Button>
                  </span>
                }
              />
            ))}
          </RowList>
        )}
        {error && <p className="text-sm text-warn">{error}</p>}
      </Section>

      {machines && machines.length > 0 && (
        <ComputeActivity
          apps={[...new Set((overview.data?.apps ?? []).map((a) => a.app))].sort()}
          machines={machines.map((m) => m.name).sort()}
        />
      )}
      {dialog}
    </SettingsPage>
  );
}

function PoolRow({ pool, machines, apps, onGrant, busy }: {
  pool: ComputePool;
  machines: ComputeMachine[];
  apps: ComputeUsage[];
  onGrant: (app: string, on: boolean) => void;
  busy: boolean;
}) {
  const names = pool.machines.map((id) => machines.find((m) => m.id === id)?.name ?? id);
  return (
    <RowItem
      title={pool.name}
      actions={
        apps.length > 0 && (
          <span className="flex flex-col items-end gap-1.5">
            {apps.map((a) => (
              <span key={a.app} className="flex items-center gap-2 text-hint text-ink-muted">
                {a.running.interactive + a.running.batch > 0 && (
                  <span>{a.running.interactive} live · {a.running.batch} batch</span>
                )}
                <Toggle label={a.app} checked={a.on} disabled={busy} onChange={(on) => onGrant(a.app, on)} />
              </span>
            ))}
          </span>
        )
      }
      meta={
        pool.online === 0 ? (
          <>Offline — {names.join(", ")} {names.length === 1 ? "isn't" : "aren't"} reachable; apps do the work themselves.</>
        ) : pool.models ? (
          <>
            {pool.models.length === 0 ? "No models yet" : pool.models.join(", ")}, on {names.join(", ")}
          </>
        ) : (
          <>
            {pool.capacity.interactive} at once, up to {pool.capacity.batch} of them batch, on {names.join(", ")}
          </>
        )
      }
    />
  );
}

function MachineFacts({ machine: m }: { machine: ComputeMachine }) {
  const inv = m.inventory;
  const memory = inv.memoryBytes ? `${Math.round(inv.memoryBytes / 2 ** 30)} GB` : "";
  const parts = [
    [inv.os, inv.arch].filter(Boolean).join("/"),
    inv.cpus ? `${inv.cpus} cores` : "",
    memory,
    inv.video ? `ffmpeg ${inv.video.ffmpeg}` : "no ffmpeg",
    inv.video?.confinement ? `confined (${inv.video.confinement})` : "",
    m.pool ? `lends ${m.slots.interactive} at once, up to ${m.slots.batch} batch` : m.why ? `not lending video: ${m.why}` : "",
    inv.busy ? "no batch work now: its owner marked it busy, or it's short of memory" : "",
    inv.version ? `fog-node ${inv.version}` : "",
  ].filter(Boolean);
  const failed = m.behind?.stuck && m.behind.error;
  return (
    <>
      {parts.join(" · ")}
      {inv.video?.unconfined && (
        <span className="block font-medium text-warn">
          Unconfined: its owner turned confinement off (fog-node unconfined on), so the ffmpeg apps hand it runs with all
          of that user's access to the machine and the network.
        </span>
      )}
      {m.clockOff && <span className="block font-medium text-warn">Not polling: {m.clockOff}.</span>}
      {failed && <span className="block text-warn">Updating to {m.behind!.want} failed: {m.behind!.error}</span>}
    </>
  );
}

// A machine's budget: how many runs it takes at once, and how many of
// those may be batch work. "Its own" is what fog-node measured it can do.
function BudgetPicker({ machine: m, disabled, onChange }: { machine: ComputeMachine; disabled: boolean; onChange: (b: Slots | null) => void }) {
  const own = m.inventory.video?.slots ?? { interactive: 0, batch: 0 };
  const current = m.budget ?? own;
  const choices = Array.from({ length: 17 }, (_, i) => i);
  return (
    <span className="flex items-center gap-1.5 text-hint text-ink-muted">
      <Select
        aria-label="Runs at once"
        value={String(current.interactive)}
        disabled={disabled}
        onChange={(e) => onChange({ interactive: Number(e.target.value), batch: Math.min(current.batch, Number(e.target.value)) })}
      >
        {choices.map((n) => <option key={n} value={n}>{n} at once</option>)}
      </Select>
      <Select
        aria-label="Of them batch"
        value={String(current.batch)}
        disabled={disabled}
        onChange={(e) => onChange({ interactive: current.interactive, batch: Number(e.target.value) })}
      >
        {choices.filter((n) => n <= current.interactive).map((n) => <option key={n} value={n}>{n} batch</option>)}
      </Select>
      {m.budget && (
        <Button size="sm" variant="ghost" disabled={disabled} onClick={() => onChange(null)} title={`Its own: ${own.interactive} at once, ${own.batch} batch`}>
          Reset
        </Button>
      )}
    </span>
  );
}
