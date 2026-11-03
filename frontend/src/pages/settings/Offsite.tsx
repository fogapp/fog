// Settings › Storage › Offsite: the locker in the cloud. One card per
// target — where it is, what the probe proved the key can and cannot do
// (the grade), what it holds and costs — and the form that adds one, with
// the provider's own setup steps and the one-way-door warning said before
// anything is created. See docs/locker-contract.md.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ApiError, del, get, post, put,
  type B2SetupReport, type OffsiteJob, type OffsiteJobStarted, type OffsitePresetsResponse, type OffsiteProbe,
  type OffsiteSaved, type OffsiteTarget, type OffsiteTargetsResponse, type RecoverResult,
} from "../../api";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import Field, { Select } from "../../components/Field";
import Toggle from "../../components/Toggle";
import { LoadError, Loading, errorText, phaseOf } from "../../components/Async";
import { formatBytes, timeAgo } from "../../lib/format";
import { useJob } from "../../lib/useJob";
import { Link } from "react-router-dom";
import { paths } from "../../lib/paths";
import { SettingsPage } from "./Frame";
import { useSettingsForm } from "./useSettingsForm";

export const offsiteKey = ["offsite-targets"] as const;

const EMPTY = {
  name: "", provider: "b2", endpoint: "", region: "", bucket: "", prefix: "",
  pathStyle: true, pricePerTb: "6.95", keyId: "", secret: "", lockerId: "", maxMbps: "", window: "",
};
type Form = typeof EMPTY;

// A bucket name nobody else has: Backblaze's names are global.
const suggestBucket = () =>
  "fog-" + Array.from(crypto.getRandomValues(new Uint8Array(5)), (b) => b.toString(16).padStart(2, "0")).join("");

function toSpec(f: Form) {
  return {
    name: f.name, provider: f.provider, endpoint: f.endpoint, region: f.region, bucket: f.bucket, prefix: f.prefix,
    pathStyle: f.pathStyle, pricePerTb: Number(f.pricePerTb) || 0, keyId: f.keyId, secret: f.secret,
    lockerId: f.lockerId, maxMbps: Number(f.maxMbps) || 0, window: f.window,
  };
}

export default function Offsite() {
  const [ask, dialog] = useConfirm();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [form, setForm] = useState<Form>(EMPTY);
  const [advanced, setAdvanced] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<{ name: string; probe: OffsiteProbe; setup?: B2SetupReport } | null>(null);
  // B2 targets default to fog making the bucket and key; "manual" is the
  // existing-bucket form.
  const [manual, setManual] = useState(false);
  const [objectLock, setObjectLock] = useState(true);

  const targets = useQuery({
    queryKey: offsiteKey,
    queryFn: () => get<OffsiteTargetsResponse>("/api/offsite/targets"),
    refetchInterval: 15_000,
  });
  const presets = useQuery({
    queryKey: ["offsite-presets"],
    queryFn: () => get<OffsitePresetsResponse>("/api/offsite/presets"),
    staleTime: Infinity,
  });
  const refresh = () => qc.invalidateQueries({ queryKey: offsiteKey });
  const preset = presets.data?.presets.find((p) => p.id === form.provider);

  const closeForm = () => {
    setAdding(false); setEditing(null); setForm(EMPTY); setError(""); setAdvanced(false); setManual(false); setObjectLock(true);
  };
  const openAdd = () => {
    const p = presets.data?.presets.find((x) => x.id === "b2");
    setForm({ ...EMPTY, name: list.length ? "" : "backblaze", bucket: suggestBucket(), region: p?.region ?? "", pricePerTb: String(p?.pricePerTb ?? 6.95), pathStyle: p?.pathStyle ?? true });
    setAdding(true);
    setEditing(null);
    setError("");
  };
  const choosePreset = (id: string) => {
    const p = presets.data?.presets.find((x) => x.id === id);
    setForm({ ...form, provider: id, region: p?.region ?? form.region, pricePerTb: String(p?.pricePerTb ?? form.pricePerTb), pathStyle: p?.pathStyle ?? form.pathStyle });
  };
  const startEdit = (t: OffsiteTarget) => {
    setForm({
      name: t.name, provider: t.provider, endpoint: t.endpoint, region: t.region, bucket: t.bucket, prefix: t.prefix,
      pathStyle: t.pathStyle, pricePerTb: String(t.pricePerTb), keyId: "", secret: "", lockerId: t.lockerId,
      maxMbps: t.maxMbps ? String(t.maxMbps) : "", window: t.window,
    });
    setEditing(t.name);
    setAdding(false);
    setError("");
  };

  const add = useMutation({
    mutationFn: () => post<OffsiteSaved>("/api/offsite/targets", toSpec(form)),
    onSuccess: async (res) => {
      await refresh();
      setResult({ name: res.target.name, probe: res.probe });
      closeForm();
    },
    onError: (e) => setError(errorText(e, "Adding the target failed")),
  });
  const setup = useMutation({
    mutationFn: () => post<OffsiteSaved>("/api/offsite/b2/setup", {
      name: form.name, keyId: form.keyId, secret: form.secret, bucket: form.bucket, prefix: form.prefix,
      objectLock, pricePerTb: Number(form.pricePerTb) || 0,
    }),
    onSuccess: async (res) => {
      await refresh();
      setResult({ name: res.target.name, probe: res.probe, setup: res.setup });
      closeForm();
    },
    onError: (e) => setError(errorText(e, "Setting up the bucket failed")),
  });
  const update = useMutation({
    mutationFn: (name: string) => put<OffsiteSaved>(`/api/offsite/targets/${name}`, toSpec(form)),
    onSuccess: async (res) => {
      await refresh();
      setResult({ name: res.target.name, probe: res.probe });
      closeForm();
    },
    onError: (e) => setError(errorText(e, "Saving the target failed")),
  });
  const probe = useMutation({
    mutationFn: (name: string) => post<OffsiteSaved>(`/api/offsite/targets/${name}/probe`),
    onSuccess: async (res) => { await refresh(); setResult({ name: res.target.name, probe: res.probe }); setError(""); },
    onError: (e) => { setError(errorText(e, "Check failed")); return refresh(); },
  });
  const remove = useMutation({
    mutationFn: (name: string) => del(`/api/offsite/targets/${name}`),
    onSuccess: () => { setError(""); setResult(null); return refresh(); },
    onError: (e) => setError(errorText(e, "Remove failed")),
  });
  const sync = useMutation({
    mutationFn: (name: string) => post(`/api/offsite/targets/${name}/sync`),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e) => { setError(errorText(e, "Sync failed to start")); return refresh(); },
  });
  // Recover everything: the second half of a full-server recovery. The
  // job's report card is polled until it lands.
  const [recoverJob, setRecoverJob] = useState<string | null>(null);
  const recover = useMutation({
    mutationFn: (v: { name: string; fromListing?: boolean }) =>
      post<OffsiteJobStarted>("/api/offsite/recover", { target: v.name, fromListing: !!v.fromListing }),
    onSuccess: (res) => { setError(""); setRecoverJob(res.job.id); return refresh(); },
    onError: async (e, v) => {
      // The index fog last wrote to this locker is gone or will not open:
      // recovering from the bucket's own listing needs a deliberate yes.
      if (e instanceof ApiError && e.status === 409 && (e.body as { code?: string } | undefined)?.code === "index_missing" && !v.fromListing) {
        const ok = await ask({
          title: "The locker's index is missing",
          body: (
            <>
              <p>{e.message}</p>
              <p>Fog can read what the bucket lists instead. If someone tampered with it, that may be missing newer backups.</p>
            </>
          ),
          action: "Recover from the bucket's listing",
        });
        if (ok) recover.mutate({ name: v.name, fromListing: true });
        return;
      }
      setError(errorText(e, "Recovery failed to start"));
    },
  });
  const copy = useMutation({
    mutationFn: (v: { from: string; to: string }) => post(`/api/offsite/targets/${v.from}/copy-to/${v.to}`),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e) => setError(errorText(e, "Copy failed to start")),
  });
  const role = useMutation({
    mutationFn: (v: { name: string; action: "retire" | "activate" }) => post(`/api/offsite/targets/${v.name}/${v.action}`),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e) => setError(errorText(e, "Change failed")),
  });
  const recoverStatus = useJob<OffsiteJob & { result?: RecoverResult }>("offsite", recoverJob, { interval: 2000 });

  const list = targets.data?.targets ?? [];
  const phase = phaseOf(targets, list.length);
  const passphraseSet = targets.data?.passphraseSet ?? true;
  const set = (k: keyof Form) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value });
  const busy = add.isPending || update.isPending || setup.isPending;
  const auto = adding && form.provider === "b2" && !manual;
  const canSubmit = auto
    ? form.name && form.bucket && form.keyId && form.secret
    : form.name && form.endpoint && form.bucket && (editing || (form.keyId && form.secret));

  return (
    <SettingsPage
      description="A sealed copy of every backup — app snapshots, the recovery kit, Drive — in a bucket you rent. The bucket is also a source: a fresh install with the credentials and your kit passphrase brings the whole server back."
      actions={
        !adding && !editing ? (
          <Button variant="primary" onClick={openAdd} disabled={!passphraseSet}>Add target</Button>
        ) : undefined
      }
    >
      {targets.data && !passphraseSet && (
        <p className="max-w-3xl rounded-lg border border-warn/30 bg-warn/10 px-4 py-3 text-sm text-warn">
          Set a <Link to={paths.settings("storage/recovery")} className="underline">recovery-kit passphrase</Link> first. It is the one secret that
          opens the locker on another machine — keep it somewhere that survives this one.
        </p>
      )}

      {list.length > 0 && (
        <ul className="max-w-3xl space-y-3">
          {list.map((t) => (
            <TargetCard key={t.name} t={t}
              onProbe={() => probe.mutate(t.name)} probing={probe.isPending && probe.variables === t.name}
              onSync={() => sync.mutate(t.name)} syncing={sync.isPending && sync.variables === t.name}
              onRecover={async () => {
                const ok = await ask({
                  title: `Recover everything from ${t.name}?`,
                  body: (
                    <>
                      <p>
                        Every app this server knows gets its newest snapshot from the locker and is restored from it — apps
                        not yet deployed are deployed with the restore staged. Drive comes back into its root.
                      </p>
                      <p className="text-warn">Current app data is replaced.</p>
                      <p>This is the step after restoring a recovery kit on a fresh machine.</p>
                    </>
                  ),
                  action: "Recover everything",
                  danger: true,
                });
                if (ok) recover.mutate({ name: t.name });
              }}
              onEdit={() => startEdit(t)}
              others={list.filter((o) => o.name !== t.name).map((o) => o.name)}
              onCopyTo={async (to) => {
                const ok = await ask({
                  title: `Copy the ${t.name} locker into ${to}?`,
                  body: (
                    <>
                      <p>
                        Every object is fetched, checked, and written under the same locker id, so {to} becomes a second home
                        of the same history — same passphrase, nothing re-encrypted. {to} must be empty.
                      </p>
                      <p>Nightly syncs write to both from then on; retire {t.name} when you are done with it.</p>
                    </>
                  ),
                  action: "Copy locker",
                });
                if (ok) copy.mutate({ from: t.name, to });
              }}
              onRole={async (action) => {
                if (action === "retire" && !(await ask({
                  title: `Retire ${t.name}?`,
                  body: "Fog stops writing to it; everything in it stays readable and restorable.",
                  action: "Retire",
                }))) return;
                role.mutate({ name: t.name, action });
              }}
              onRemove={async () => {
                const ok = await ask({
                  title: `Forget target ${t.name}?`,
                  body: "Fog stops writing to it. Nothing in the bucket is touched — fog holds no right to empty it.",
                  action: "Forget",
                  danger: true,
                });
                if (ok) remove.mutate(t.name);
              }} />
          ))}
        </ul>
      )}
      {phase === "loading" && <Loading label="Loading offsite targets…" />}
      {phase === "error" && <LoadError label="Couldn't load offsite targets." error={targets.error} onRetry={() => targets.refetch()} />}
      {phase === "empty" && !adding && passphraseSet && (
        <p className="text-xs text-ink-muted">No offsite target yet — backups stay in the house.</p>
      )}
      {list.length > 0 && <RetentionForm />}

      {result && !adding && !editing && (
        <ProbeReport name={result.name} probe={result.probe} setup={result.setup} onClose={() => setResult(null)} />
      )}
      {recoverJob && recoverStatus.data && (
        <RecoverReport job={recoverStatus.data} onClose={() => setRecoverJob(null)} />
      )}

      {(adding || editing) && (
        <div className="max-w-3xl space-y-4 rounded-lg border border-veil-edge bg-veil p-4">
          {editing && (
            <p className="text-xs text-ink-muted">
              Editing <span className="font-medium text-ink">{editing}</span> — the stored key is kept unless you
              enter a new pair. Saving re-checks the bucket; moving to another bucket or prefix finds or creates a locker there.
            </p>
          )}
          <div className="grid grid-cols-2 gap-4">
            <Select label="Provider" value={form.provider} onChange={(e) => choosePreset(e.target.value)}>
              {(presets.data?.presets ?? []).map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
            </Select>
            <Field label="Name" value={form.name} onChange={set("name")} placeholder="backblaze" disabled={Boolean(editing)}
              hint="Lowercase letters, digits and dashes." />
          </div>

          {auto ? (
            <div className="space-y-4">
              <div className="space-y-2 rounded-lg border border-hair bg-fog-950/40 p-3 text-xs text-ink-muted">
                <p className="font-medium text-ink">Fog makes the bucket and a key that cannot destroy what it writes.</p>
                <p>
                  Backblaze's console only makes keys that can delete; its API can make one that can't. Give fog your
                  master application key and it creates a private bucket with a lifecycle rule that clears hidden files,
                  plus a key for that bucket alone that can read, write, hide and lock. Fog keeps only that key. The master
                  key is used for this one request and never stored or logged.
                </p>
                <p>
                  Get it in the Backblaze console under Application Keys → <span className="text-ink">Generate New Master Application Key</span>.
                  A new master key replaces the old one, so anything still using the old one stops working. Generate another
                  afterwards if you want this one retired too.
                </p>
              </div>
              <div className="grid grid-cols-2 gap-4">
                <Field label="Master key id" value={form.keyId} onChange={set("keyId")} autoComplete="off" />
                <Field label="Master application key" type="password" value={form.secret} onChange={set("secret")}
                  autoComplete="off" placeholder="used once, not stored" />
              </div>
              <div className="grid grid-cols-2 gap-4">
                <Field label="Bucket" value={form.bucket} onChange={set("bucket")} placeholder="fog-…"
                  hint="Bucket names are shared across all of Backblaze, so this one is random. Change it if you like." />
                <Field label="Storage price, $ per TB-month" value={form.pricePerTb} onChange={set("pricePerTb")} placeholder="6.95"
                  hint="For the cost meter only." />
              </div>
              <div className="space-y-2">
                <Toggle checked={objectLock} onChange={setObjectLock} label="Object Lock (recommended)" />
                {objectLock ? (
                  preset?.warning && (
                    <p className="rounded-md border border-warn/30 bg-warn/10 px-3 py-2 text-xs text-warn">
                      <span className="font-semibold">Read before creating the bucket. </span>{preset.warning}
                    </p>
                  )
                ) : (
                  <p className="text-xs text-ink-muted">
                    Without Object Lock, the history is only as safe as the key fog keeps and your Backblaze login: anyone
                    signed in to the account can still erase it. You can turn Object Lock on later in the bucket's settings.
                  </p>
                )}
              </div>
              <Button variant="ghost" size="sm" onClick={() => { setManual(true); setForm({ ...form, keyId: "", secret: "", bucket: "" }); }}>
                I already have a bucket and key
              </Button>
            </div>
          ) : (
            <>
          {adding && form.provider === "b2" && (
            <Button variant="ghost" size="sm" onClick={() => { setManual(false); setForm({ ...form, keyId: "", secret: "", bucket: suggestBucket() }); }}>
              Let fog set up the bucket from my master key instead
            </Button>
          )}
          {preset && !editing && (
            <div className="space-y-2 rounded-lg border border-hair bg-fog-950/40 p-3 text-xs text-ink-muted">
              <p className="font-medium text-ink">On {preset.label}'s side, before you fill this in:</p>
              <ol className="list-decimal space-y-1 pl-5">
                {preset.setup.map((s, i) => <li key={i}>{s}</li>)}
              </ol>
              {preset.warning && (
                <p className="rounded-md border border-warn/30 bg-warn/10 px-3 py-2 text-warn">
                  <span className="font-semibold">Read before enabling Object Lock. </span>{preset.warning}
                </p>
              )}
            </div>
          )}

          <div className="grid grid-cols-2 gap-4">
            <Field label="Endpoint" value={form.endpoint} onChange={set("endpoint")} placeholder={preset?.endpoint ?? "https://…"} />
            <Field label="Region" value={form.region} onChange={set("region")} placeholder={preset?.region ?? ""} />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <Field label="Bucket" value={form.bucket} onChange={set("bucket")} placeholder="fog-backups" />
            <Field label="Prefix" value={form.prefix} onChange={set("prefix")} placeholder="optional, e.g. house"
              hint="A path inside the bucket. The locker lives under it as fog-locker/1/<id>/." />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <Field label={editing ? "New key id (optional)" : "Key id"} value={form.keyId} onChange={set("keyId")} autoComplete="off" />
            <Field label={editing ? "New secret (optional)" : "Secret"} type="password" value={form.secret} onChange={set("secret")}
              autoComplete="new-password" placeholder="stored encrypted" />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <Field label="Storage price, $ per TB-month" value={form.pricePerTb} onChange={set("pricePerTb")} placeholder="6.95"
              hint="For the cost meter only." />
          </div>
          <div>
            <Button variant="ghost" size="sm" onClick={() => setAdvanced(!advanced)} aria-expanded={advanced}>
              {advanced ? "Hide advanced" : "Advanced"}
            </Button>
          </div>
          {advanced && (
            <div className="space-y-4">
              <div className="grid grid-cols-2 gap-4">
                <Field label="Upload cap, Mbit/s" value={form.maxMbps} onChange={set("maxMbps")} placeholder="0 = no cap" />
                <Field label="Upload window (UTC)" value={form.window} onChange={set("window")} placeholder="e.g. 01:00-07:00, empty = any time" />
              </div>
              <div className="grid grid-cols-2 gap-4">
                <Field label="Locker id" value={form.lockerId} onChange={set("lockerId")} placeholder="only if the prefix holds several"
                  hint="Empty adopts the one locker under the prefix, or creates a new one." />
                <div className="space-y-1.5 pt-6">
                  <Toggle checked={form.pathStyle} onChange={(on) => setForm({ ...form, pathStyle: on })} label="Path-style addressing" />
                  <p className="text-hint text-ink-muted">endpoint/bucket/key — what B2, R2, Wasabi and MinIO expect.</p>
                </div>
              </div>
            </div>
          )}
            </>
          )}
          <div className="flex items-center gap-3">
            {editing ? (
              <Button variant="primary" onClick={() => update.mutate(editing)} disabled={busy || !canSubmit}>
                {update.isPending ? "Checking…" : "Save & check"}
              </Button>
            ) : auto ? (
              <Button variant="primary" onClick={() => setup.mutate()} disabled={busy || !canSubmit}>
                {setup.isPending ? "Setting up…" : "Set up & check"}
              </Button>
            ) : (
              <Button variant="primary" onClick={() => add.mutate()} disabled={busy || !canSubmit}>
                {add.isPending ? "Checking the bucket…" : "Add & check"}
              </Button>
            )}
            <Button onClick={closeForm}>Cancel</Button>
            {busy && (
              <span className="text-xs text-ink-muted">
                {setup.isPending
                  ? "Creating the bucket and a restricted key, then checking what that key can delete…"
                  : "Writing a test object, checking what the key may delete, creating the locker…"}
              </span>
            )}
          </div>
        </div>
      )}
      {error && <p className="max-w-3xl break-words text-sm text-warn">{error}</p>}
      {dialog}
    </SettingsPage>
  );
}

const GRADE_STYLE: Record<string, string> = {
  A: "bg-accent/10 text-accent border-accent/30",
  B: "bg-accent/10 text-accent border-accent/30",
  C: "bg-warn/10 text-warn border-warn/30",
  D: "bg-warn/10 text-warn border-warn/30",
};

function GradePill({ grade, label }: { grade: string; label: string }) {
  const style = GRADE_STYLE[grade] ?? "bg-fog-800 text-ink-muted border-edge";
  return (
    <span className={`inline-flex items-center gap-1.5 rounded border px-2 py-0.5 text-[11px] font-semibold ${style}`}>
      <span className="text-sm leading-none">{grade || "?"}</span>
      {label}
    </span>
  );
}

// The locker's retention tiers: counted in backups, so the same numbers
// scale with the schedule's cadence.
function RetentionForm() {
  const { value, set, SaveButton, loading } = useSettingsForm(["offsite_keep_recent", "offsite_keep_monthly", "offsite_keep_yearly"]);
  return (
    <div className="max-w-3xl space-y-3 rounded-lg border border-veil-edge bg-veil p-4">
      <p className="text-sm font-medium text-ink">What the locker keeps</p>
      <p className="text-xs text-ink-muted">
        Counted in backups, not days — nightly backups make 30 recent a month; weekly backups make it seven months.
        Beyond the recent set, the first backup of each month, then of each year, stays. Nothing is destroyed:
        what falls out of the policy is hidden, and the bucket's lifecycle rule frees the space later.
      </p>
      {loading ? (
        <Loading label="Loading settings…" />
      ) : (
        <div className="grid grid-cols-3 gap-4">
          <Field label="Recent" value={value("offsite_keep_recent")} onChange={set("offsite_keep_recent")} placeholder="30" hint="newest backups" />
          <Field label="Monthly" value={value("offsite_keep_monthly")} onChange={set("offsite_keep_monthly")} placeholder="6" hint="first of each month" />
          <Field label="Yearly" value={value("offsite_keep_yearly")} onChange={set("offsite_keep_yearly")} placeholder="3" hint="first of each year" />
        </div>
      )}
      {SaveButton}
    </div>
  );
}

function RecoverReport({ job, onClose }: { job: OffsiteJob & { result?: RecoverResult }; onClose: () => void }) {
  const res = job.result;
  const steps = [...(res?.apps ?? []), ...(res?.files ? [res.files] : [])];
  return (
    <div className="max-w-3xl space-y-2 rounded-lg border border-veil-edge bg-veil px-4 py-3 text-sm">
      <p className="flex items-center gap-2 text-ink">
        <span className="font-medium">{job.label}</span>
        <span className={job.state === "running" ? "text-accent" : job.state === "done" ? "text-accent" : "text-warn"}>
          {job.state === "running" ? (job.current || "working…") : job.state === "done" ? "finished" : job.error || job.state}
        </span>
        {job.state !== "running" && <Button variant="ghost" size="sm" className="ml-auto" onClick={onClose}>Dismiss</Button>}
      </p>
      {steps.length > 0 && (
        <ul className="space-y-0.5 text-xs">
          {steps.map((st) => (
            <li key={st.name} className="flex gap-2">
              <span className={st.status === "restored" ? "text-accent" : st.status === "failed" ? "text-warn" : "text-ink-muted"}>
                {st.status === "restored" ? "✓" : st.status === "failed" ? "✗" : "○"}
              </span>
              <span className="text-ink">{st.name}</span>
              {st.stamp && <span className="text-ink-muted">{st.stamp}</span>}
              <span className="text-ink-muted">{st.status}{st.detail ? ` — ${st.detail}` : ""}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function TargetCard({ t, onProbe, probing, onSync, syncing, onRecover, onEdit, onRemove, others, onCopyTo, onRole }: {
  t: OffsiteTarget; onProbe: () => void; probing: boolean; onSync: () => void; syncing: boolean; onRecover: () => void; onEdit: () => void; onRemove: () => void;
  others: string[]; onCopyTo: (to: string) => void; onRole: (action: "retire" | "activate") => void;
}) {
  const [showSteps, setShowSteps] = useState(false);
  const [copyTo, setCopyTo] = useState("");
  const p = t.probeResult;
  const held = t.totals.storedBytes + t.totals.hiddenBytes;
  const job = t.job;
  return (
    <li className={`space-y-2 rounded-lg border px-4 py-3 ${t.role === "retired" ? "border-veil-edge bg-veil opacity-75" : "border-veil-edge bg-veil"}`}>
      <div className="flex flex-wrap items-center gap-3">
        <div className="min-w-0 flex-1">
          <p className="flex flex-wrap items-center gap-2 text-sm font-medium text-ink">
            {t.name}
            <GradePill grade={t.grade} label={t.gradeLabel} />
            {t.role === "retired" && <span className="rounded-full border border-hair px-2 py-px text-[11px] font-medium text-ink-muted">Retired</span>}
          </p>
          <p className="truncate font-mono text-xs text-ink-muted">{t.bucket}{t.prefix ? `/${t.prefix}` : ""} · {t.endpoint.replace(/^https?:\/\//, "")}</p>
        </div>
        <div className="flex shrink-0 flex-wrap justify-end gap-2">
          <Button size="sm" onClick={onSync} disabled={syncing || Boolean(job) || t.role !== "active"}>
            {job ? "Syncing…" : syncing ? "Starting…" : "Sync now"}
          </Button>
          <Button size="sm" onClick={onProbe} disabled={probing}>{probing ? "Checking…" : "Re-check"}</Button>
          <Button size="sm" onClick={onEdit}>Edit</Button>
          {t.role === "active" ? (
            <Button size="sm" onClick={() => onRole("retire")}>Retire</Button>
          ) : (
            <Button size="sm" onClick={() => onRole("activate")}>Reactivate</Button>
          )}
          <Button size="sm" variant="danger" onClick={onRecover} disabled={Boolean(job)}
            title="Restore every app and Drive from this locker — after a recovery kit made this box the old server">
            Recover everything…
          </Button>
          <Button size="sm" variant="danger" onClick={onRemove}>Forget</Button>
        </div>
      </div>
      {others.length > 0 && t.totals.stored > 0 && (
        <p className="flex items-center gap-2 text-xs text-ink-muted">
          <span>Copy this locker to</span>
          <Select value={copyTo} onChange={(e) => setCopyTo(e.target.value)} aria-label="Target to copy into">
            <option value="">choose a target…</option>
            {others.map((o) => <option key={o} value={o}>{o}</option>)}
          </Select>
          <Button size="sm" disabled={!copyTo || Boolean(job)} onClick={() => { onCopyTo(copyTo); setCopyTo(""); }}>Copy</Button>
          <span className="text-ink-muted">— the way to move providers, or to keep two.</span>
        </p>
      )}
      <p className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-muted">
        <span>{t.totals.stored.toLocaleString()} objects · {formatBytes(held)} held</span>
        {t.totals.hiddenBytes > 0 && <span>{formatBytes(t.totals.hiddenBytes)} hidden, awaiting expiry</span>}
        <span>about ${t.monthlyCost < 0.005 && held > 0 ? "<0.01" : t.monthlyCost.toFixed(2)} a month</span>
        {t.lastSyncAt ? <span>last synced {timeAgo(t.lastSyncAt)}</span> : <span>never synced</span>}
        {t.probedAt && <span>checked {timeAgo(t.probedAt)}</span>}
      </p>
      {job && (
        <p className="text-xs text-accent">
          {job.current || "syncing"}
          {job.total > 0 && ` · ${formatBytes(job.done)} of ${formatBytes(job.total)}`}
          {job.items > 0 && ` · ${job.items} object${job.items === 1 ? "" : "s"} copied`}
        </p>
      )}
      {t.lastError && !job && <p className="break-words font-mono text-xs text-warn">{t.lastError}</p>}
      {p?.fix && (
        <p className="rounded-md border border-warn/30 bg-warn/10 px-3 py-2 text-xs text-warn">
          <span className="font-semibold">To raise the grade: </span>{p.fix}
        </p>
      )}
      {p && (
        <Button variant="ghost" size="sm" onClick={() => setShowSteps(!showSteps)} aria-expanded={showSteps}>
          {showSteps ? "Hide what the check found" : "What the check found"}
        </Button>
      )}
      {p && showSteps && <ProbeSteps probe={p} />}
    </li>
  );
}

function ProbeSteps({ probe }: { probe: OffsiteProbe }) {
  return (
    <div className="space-y-1 text-xs">
      <ul className="space-y-0.5">
        {probe.steps.map((s) => (
          <li key={s.name} className="flex gap-2">
            <span className={s.ok ? "text-accent" : "text-ink-muted"}>{s.ok ? "✓" : "○"}</span>
            <span className="text-ink-muted">{s.name}</span>
            {s.detail && <span className="text-ink-muted">— {s.detail}</span>}
          </li>
        ))}
      </ul>
      {probe.recommendations?.map((r, i) => <p key={i} className="text-ink-muted">Recommended: {r}</p>)}
    </div>
  );
}

function ProbeReport({ name, probe, setup, onClose }: { name: string; probe: OffsiteProbe; setup?: B2SetupReport; onClose: () => void }) {
  return (
    <div className="max-w-3xl space-y-2 rounded-lg border border-veil-edge bg-veil px-4 py-3 text-sm">
      <p className="flex items-center gap-2 text-ink">
        <span className="font-medium">{name}</span> checked — <GradePill grade={probe.grade} label={gradeWord(probe.grade)} />
        <Button variant="ghost" size="sm" className="ml-auto" onClick={onClose}>Dismiss</Button>
      </p>
      {setup && <SetupSummary setup={setup} />}
      {probe.grade === "A" ? (
        probe.lockHolds ? (
          <p className="text-xs text-ink-muted">The key could delete an unlocked version, but everything fog writes is locked in compliance mode and stays locked while it's retained. Neither this machine, this key, nor your account login can erase the retained history.</p>
        ) : (
          <p className="text-xs text-ink-muted">The key can write, read and list but cannot destroy a version, and what fog writes is locked. Neither this machine, this key, nor your account login can erase the retained history.</p>
        )
      ) : (
        <p className="text-xs text-warn"><span className="font-semibold">To raise the grade: </span>{probe.fix}</p>
      )}
      <ProbeSteps probe={probe} />
    </div>
  );
}

// What fog did on Backblaze's side, so the operator can find it there.
function SetupSummary({ setup }: { setup: B2SetupReport }) {
  return (
    <div className="space-y-1 text-xs text-ink-muted">
      <p>
        {setup.adopted ? "Used your existing bucket " : "Created the private bucket "}
        <span className="font-mono text-ink">{setup.bucket}</span>
        {setup.objectLock ? ", with Object Lock" : ", without Object Lock"}
        {setup.lifecycleDays > 0
          ? `; hidden files are deleted ${setup.lifecycleDays} days after fog hides them.`
          : "; it has no lifecycle rule, so hidden files are kept until you delete them."}
      </p>
      <p>
        Fog keeps the key <span className="font-mono text-ink">{setup.keyName}</span>, limited to that bucket:{" "}
        <span className="text-ink-muted">{setup.capabilities.join(", ")}</span>. The master key was not kept.
      </p>
      {setup.deleteFallback && (
        <p className="text-warn">
          Backblaze wouldn't let a key without deleteFiles hide files through its S3 interface, so the key fog keeps has it.
          {setup.objectLock ? " Object Lock still covers everything fog writes." : " Without Object Lock this key could erase history."}
        </p>
      )}
    </div>
  );
}

function gradeWord(g: string) {
  return { A: "Immutable", B: "Append-only", C: "Versioned", D: "Plain" }[g] ?? "Unknown";
}
