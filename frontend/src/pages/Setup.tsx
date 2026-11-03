import { useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { post, postForm, put, type LockerPreview } from "../api";
import DockerInstall from "../components/DockerInstall";
import Field from "../components/Field";
import Logo from "../components/Logo";
import { errorText } from "../components/Async";
import { usePollUntilRestart } from "../lib/usePollUntilRestart";
import { meQuery, settingsQuery, setupStatusQuery, systemQuery } from "../queries";

type Step = "admin" | "recovery" | "docker" | "domain";

// First-run wizard: create the admin account, set the recovery-kit
// passphrase, get Docker running, set the base domain. Each step is
// skippable except the account.
//
// Recovery comes right after the account because the account step is what
// mints the vault master key — from this moment on there is something worth
// a kit, and the nightly backups can't write one without the passphrase.
export default function Setup() {
  const [step, setStep] = useState<Step>("admin");
  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <div className="w-full max-w-md">
        <h1 className="mb-1 flex items-center justify-center gap-2 text-2xl font-bold tracking-[-.015em] text-ink">
          <Logo className="h-10" />
          fog
        </h1>
        <p className="mb-8 text-center text-sm text-ink-muted">
          Set up your server&apos;s new control plane.
        </p>
        <div className="rounded-xl border border-veil-edge bg-veil p-6">
          <StepIndicator current={step} />
          {step === "admin" && <AdminStep onDone={() => setStep("recovery")} />}
          {step === "recovery" && <RecoveryStep onDone={() => setStep("docker")} />}
          {step === "docker" && <DockerStep onDone={() => setStep("domain")} />}
          {step === "domain" && <DomainStep />}
        </div>
      </div>
    </div>
  );
}

function StepIndicator({ current }: { current: Step }) {
  const steps: [Step, string][] = [
    ["admin", "Account"],
    ["recovery", "Recovery"],
    ["docker", "Docker"],
    ["domain", "Domain"],
  ];
  return (
    <div className="mb-6 flex gap-2">
      {steps.map(([key, label]) => (
        <div
          key={key}
          className={`flex-1 rounded-full px-2 py-1 text-center text-xs font-medium ${
            key === current ? "bg-accent text-accent-on" : "bg-fog-800 text-ink-muted"
          }`}
        >
          {label}
        </div>
      ))}
    </div>
  );
}

function AdminStep({ onDone }: { onDone: () => void }) {
  const qc = useQueryClient();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [serverName, setServerName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  // Full-server DR door: restore an encrypted recovery kit instead of
  // starting fresh. fogd restores its state and restarts; the page reloads
  // into the recovered server's login.
  const [restoreOpen, setRestoreOpen] = useState(false);
  const [kitPassphrase, setKitPassphrase] = useState("");
  const kitRef = useRef<HTMLInputElement>(null);
  // The other DR door: the kit is in an offsite locker. Credentials and
  // the passphrase are enough; fog looks first, then restores the newest
  // kit exactly as an uploaded one.
  const [cloudOpen, setCloudOpen] = useState(false);
  const [cloud, setCloud] = useState({ endpoint: "", region: "", bucket: "", prefix: "", keyId: "", secret: "", passphrase: "", lockerId: "" });
  const [preview, setPreview] = useState<LockerPreview | null>(null);
  // A legacy kit cannot prove its date; restoring one needs an explicit
  // go-ahead from the operator, who has read the warning.
  const [acceptLegacyKit, setAcceptLegacyKit] = useState(false);
  const setCloudField = (k: keyof typeof cloud) => (e: React.ChangeEvent<HTMLInputElement>) => setCloud({ ...cloud, [k]: e.target.value });
  // Consent names the exact kit the preview showed (name and digest): a kit
  // swapped in afterwards does not inherit it.
  const cloudBody = (dryRun: boolean) => ({
    ...cloud, name: "offsite", pathStyle: true, dryRun,
    acceptLegacyKit: acceptLegacyKit && preview?.newestKit ? { name: preview.newestKit.name, sha256: preview.newestKit.sha256 ?? "" } : null,
    // The restore opens exactly the kit the preview showed, or nothing.
    kit: !dryRun && preview?.newestKit && !preview.newestKit.error ? { name: preview.newestKit.name, sha256: preview.newestKit.sha256 ?? "" } : null,
  });

  async function lookInLocker() {
    setBusy(true);
    setError("");
    setPreview(null);
    setAcceptLegacyKit(false);
    try {
      setPreview(await post<LockerPreview>("/api/setup/restore-offsite", cloudBody(true)));
    } catch (err) {
      setError(errorText(err, "Couldn't open the locker"));
    } finally {
      setBusy(false);
    }
  }

  async function restoreFromLocker() {
    setBusy(true);
    setError("");
    try {
      await post("/api/setup/restore-offsite", cloudBody(false));
      restart.begin({ intervalMs: 3000, deadlineMs: 3 * 60_000 });
    } catch (err) {
      setError(errorText(err, "Restore failed"));
      setBusy(false);
    }
  }
  // Down-then-up: a poll landing before fogd actually restarts must not
  // reload into the OLD process while the screen claims "Restoring…".
  const restart = usePollUntilRestart();

  async function restoreKit() {
    const file = kitRef.current?.files?.[0];
    if (!file) {
      setError("Choose a recovery kit (.enc) first");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const form = new FormData();
      form.append("kit", file);
      form.append("passphrase", kitPassphrase);
      await postForm("/api/setup/restore-kit", form);
      restart.begin({ intervalMs: 3000, deadlineMs: 3 * 60_000 });
    } catch (err) {
      setError(errorText(err, "Restore failed"));
      setBusy(false);
    }
  }

  if (restart.state !== "idle") {
    return (
      <div className="space-y-2 py-6 text-center">
        <p className="text-sm font-semibold text-ink">Restoring…</p>
        {restart.timedOut ? (
          <p className="text-sm text-warn">
            fog hasn&apos;t come back yet. The restore may still be running — reload this page
            manually in a minute and sign in with the recovered admin account.
          </p>
        ) : (
          <p className="text-sm text-ink-muted">
            fog is restarting with the recovered state. This page reloads when
            it&apos;s back — sign in with the recovered admin account.
          </p>
        )}
      </div>
    );
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (password !== confirm) {
      setError("Passwords do not match");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await post("/api/setup", { username, password, serverName });
      // The setup response sets the session cookie; the app is now authed.
      await qc.invalidateQueries({ queryKey: setupStatusQuery.queryKey });
      await qc.invalidateQueries({ queryKey: meQuery.queryKey });
      onDone();
    } catch (err) {
      setError(errorText(err, "Setup failed"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} className="space-y-4">
      {/* This name is a handle: it is what you sign in with, what the
          household sees in chat, and what an audit line names. Everyone on
          this fog gets one, so it answers the same rules theirs do. */}
      <Field label="Handle" value={username} onChange={(e) => setUsername(e.target.value.toLowerCase())} autoFocus required
        hint="Lowercase letters, digits, '.', '_' or '-'; 2 to 32 characters." />
      <Field label="Password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} hint="At least 10 characters." required />
      <Field label="Confirm password" type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required />
      <Field label="Server name (optional)" value={serverName} onChange={(e) => setServerName(e.target.value)} placeholder="basement-server" />
      {error && <p className="text-sm text-warn">{error}</p>}
      <button
        type="submit"
        disabled={busy}
        className="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50"
      >
        {busy ? "Creating…" : "Create admin account"}
      </button>

      <div className="border-t border-hair pt-4">
        <button type="button" onClick={() => setRestoreOpen((v) => !v)}
          className="w-full text-center text-xs text-ink-muted hover:text-ink">
          {restoreOpen ? "Hide recovery" : "Recovering this server? Restore a recovery kit"}
        </button>
        {restoreOpen && (
          <div className="mt-3 space-y-3">
            <p className="text-xs text-ink-muted">
              The encrypted kit fog wrote beside your backups (fog-recovery-kit-*.enc)
              restores the vault master key, apps, and settings of the old server.
            </p>
            <input ref={kitRef} type="file" accept=".enc"
              className="block w-full text-xs text-ink-muted file:mr-3 file:rounded file:border file:border-edge file:bg-fog-800 file:px-3 file:py-1.5 file:text-xs file:text-ink" />
            <Field label="Kit passphrase" type="password" value={kitPassphrase}
              onChange={(e) => setKitPassphrase(e.target.value)} />
            <button type="button" onClick={restoreKit} disabled={busy}
              className="w-full rounded-md border border-edge py-2 text-sm font-semibold text-ink hover:bg-fog-800 disabled:opacity-50">
              {busy ? "Restoring…" : "Restore recovery kit"}
            </button>
          </div>
        )}
        <button type="button" onClick={() => setCloudOpen((v) => !v)}
          className="mt-3 w-full text-center text-xs text-ink-muted hover:text-ink">
          {cloudOpen ? "Hide offsite recovery" : "The kit is in an offsite locker? Restore from the cloud"}
        </button>
        {cloudOpen && (
          <div className="mt-3 space-y-3">
            <p className="text-xs text-ink-muted">
              The bucket fog backed up to, its key, and the kit passphrase. Fog opens the locker, shows what is in it,
              and restores the newest kit; afterwards, Backups → Offsite → Recover everything brings the apps and Drive back.
            </p>
            <div className="grid grid-cols-2 gap-3">
              <Field label="Endpoint" value={cloud.endpoint} onChange={setCloudField("endpoint")} placeholder="https://s3.us-west-004.backblazeb2.com" />
              <Field label="Region" value={cloud.region} onChange={setCloudField("region")} placeholder="us-west-004" />
              <Field label="Bucket" value={cloud.bucket} onChange={setCloudField("bucket")} />
              <Field label="Prefix" value={cloud.prefix} onChange={setCloudField("prefix")} placeholder="optional" />
              <Field label="Key id" value={cloud.keyId} onChange={setCloudField("keyId")} autoComplete="off" />
              <Field label="Secret" type="password" value={cloud.secret} onChange={setCloudField("secret")} autoComplete="new-password" />
            </div>
            <Field label="Kit passphrase" type="password" value={cloud.passphrase} onChange={setCloudField("passphrase")} />
            <Field label="Locker id (only if the prefix holds several)" value={cloud.lockerId} onChange={setCloudField("lockerId")} />
            {preview && (
              <div className="rounded-md border border-hair bg-fog-950/40 p-3 text-xs text-ink-muted">
                <p className="font-medium text-ink">Locker “{preview.name}” · created {new Date(preview.createdAt).toLocaleDateString()}</p>
                <p>{preview.kits.length} recovery kit{preview.kits.length === 1 ? "" : "s"}{preview.kits[0] ? ` · newest ${preview.kits[0].stamp}` : ""}</p>
                <p>{preview.apps.length > 0 ? `Apps: ${preview.apps.join(", ")}` : "No app snapshots yet"}</p>
                <p>{preview.files.length > 0 ? `Drive snapshots: ${preview.files.length}` : "No Drive snapshots"}</p>
                {preview.index?.fellBack && (
                  <div className="mt-2 rounded border border-warn/30 bg-warn/10 p-2 text-ink">
                    <p className="font-medium">The newest index in this locker could not be read</p>
                    <p>The apps and Drive snapshots above come from an older one, so newer backups may be missing from this list. Someone with access to the bucket may have tampered with it.</p>
                  </div>
                )}
                {preview.newestKit?.fellBack && !preview.newestKit.error && (
                  <div className="mt-2 rounded border border-warn/30 bg-warn/10 p-2 text-ink">
                    <p className="font-medium">This is not the newest recovery kit</p>
                    <p>
                      {preview.newestKit.latest && preview.newestKit.latest !== preview.newestKit.name
                        ? `fog's newest kit, ${preview.newestKit.latest}, could not be used`
                        : "A newer kit in the bucket could not be used"}
                      {`; restoring ${preview.newestKit.name} brings this machine back as it was then — secrets and settings changed since are lost.`}
                    </p>
                  </div>
                )}
                {(preview.newestKit?.skipped ?? []).map((sk) => (
                  <p key={sk.name} className="mt-1 text-warn">Skipped {sk.name}: {sk.reason}</p>
                ))}
                {preview.newestKit?.error && (
                  <p className="mt-2 text-warn">The newest kit can't be used: {preview.newestKit.error}</p>
                )}
                {preview.newestKit && !preview.newestKit.error && !preview.newestKit.legacy && preview.newestKit.date && (
                  <p>Kit sealed {new Date(preview.newestKit.date).toLocaleString()}</p>
                )}
                {preview.newestKit?.legacy && (
                  <div className="mt-2 rounded border border-warn/30 bg-warn/10 p-2 text-ink">
                    <p className="font-medium">Legacy kit — its date can't be verified</p>
                    <p>
                      Written by an older fog{preview.newestKit.date ? `, dated ${new Date(preview.newestKit.date).toLocaleString()} inside` : ""}: {preview.newestKit.warning}
                    </p>
                    <label className="mt-1 flex items-center gap-2">
                      <input type="checkbox" checked={acceptLegacyKit} onChange={(e) => setAcceptLegacyKit(e.target.checked)} />
                      I've checked that date; restore this kit anyway
                    </label>
                  </div>
                )}
              </div>
            )}
            <div className="flex gap-2">
              <button type="button" onClick={lookInLocker} disabled={busy || !cloud.endpoint || !cloud.bucket || !cloud.keyId || !cloud.secret || !cloud.passphrase}
                className="flex-1 rounded-md border border-edge py-2 text-sm font-semibold text-ink hover:bg-fog-800 disabled:opacity-50">
                {busy && !preview ? "Opening the locker…" : "Look in the locker"}
              </button>
              <button type="button" onClick={restoreFromLocker} disabled={busy || !preview || preview.kits.length === 0 || !!preview.newestKit?.error || (!!preview.newestKit?.legacy && !acceptLegacyKit)}
                className="flex-1 rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
                {busy && preview ? "Restoring…" : "Restore from this locker"}
              </button>
            </div>
          </div>
        )}
      </div>
    </form>
  );
}

// Mirrors the server rule (editableSettings): the kit seals the vault
// master key, so a short passphrase is refused rather than merely warned.
const KIT_PASSPHRASE_MIN = 12;

function RecoveryStep({ onDone }: { onDone: () => void }) {
  const qc = useQueryClient();
  const [passphrase, setPassphrase] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (passphrase.length < KIT_PASSPHRASE_MIN) {
      setError(`Use at least ${KIT_PASSPHRASE_MIN} characters`);
      return;
    }
    if (passphrase !== confirm) {
      setError("Passphrases do not match");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await put("/api/settings", { recovery_kit_passphrase: passphrase });
      await qc.invalidateQueries({ queryKey: settingsQuery.queryKey });
      onDone();
    } catch (err) {
      setError(errorText(err, "Saving failed"));
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} className="space-y-4">
      <p className="text-sm text-ink-muted">
        fog encrypts every app&apos;s credentials with a master key that lives only on this
        machine. With a passphrase set, it writes an encrypted <em>recovery kit</em> — master
        key + database — beside the nightly backups, so a dead disk is a restore, not a loss.
      </p>
      <Field label="Recovery-kit passphrase" type="password" value={passphrase}
        onChange={(e) => setPassphrase(e.target.value)} autoFocus
        autoComplete="new-password" hint={`At least ${KIT_PASSPHRASE_MIN} characters.`} />
      <Field label="Confirm passphrase" type="password" value={confirm}
        onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" />
      <p className="text-xs text-ink-muted">
        Keep it somewhere that survives this machine — a password manager, a note in a
        drawer. Without it the kit can&apos;t be opened, and fog can&apos;t recover it for you.
      </p>
      {error && <p className="text-sm text-warn">{error}</p>}
      <button
        type="submit"
        disabled={busy}
        className="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50"
      >
        {busy ? "Saving…" : "Save passphrase"}
      </button>
      <button
        type="button"
        onClick={onDone}
        disabled={busy}
        className="w-full rounded-md border border-edge py-2 text-sm font-medium text-ink hover:bg-fog-800 disabled:opacity-50"
      >
        Skip for now
      </button>
      <p className="text-center text-xs text-ink-muted">
        Skipping leaves the kit unscheduled — fog keeps a reminder in the Inbox, and the passphrase lives under Settings → Storage → Recovery kit.
      </p>
    </form>
  );
}

function DockerStep({ onDone }: { onDone: () => void }) {
  // Same query key as the shared install panel below, so this is one probe,
  // not two — react-query dedupes it.
  const system = useQuery({
    ...systemQuery,
    refetchInterval: 3000,
  });
  const available = system.data?.docker.available ?? false;
  const probing = !system.data && !system.isError;

  return (
    <div className="space-y-4">
      <DockerInstall buttonClassName="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover" />
      <button
        onClick={onDone}
        className="w-full rounded-md border border-edge py-2 text-sm font-medium text-ink hover:bg-fog-800"
      >
        {probing || available ? "Continue" : "Skip for now"}
      </button>
    </div>
  );
}

function DomainStep() {
  const qc = useQueryClient();
  const [domain, setDomain] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function finish(skip: boolean) {
    setBusy(true);
    setError("");
    try {
      if (!skip && domain) {
        await put("/api/settings", { base_domain: domain });
      }
      await qc.invalidateQueries();
      window.location.href = "/";
    } catch (err) {
      setError(errorText(err, "Saving failed"));
      setBusy(false);
    }
  }

  return (
    <div className="space-y-4">
      <Field
        label="Base domain (optional)"
        value={domain}
        onChange={(e) => setDomain(e.target.value)}
        placeholder="home.example.com"
        hint="Apps will get names like app.home.example.com. TLS setup arrives with the proxy — you can set or change this later in Settings."
      />
      {error && <p className="text-sm text-warn">{error}</p>}
      <button
        onClick={() => finish(false)}
        disabled={busy}
        className="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50"
      >
        Finish setup
      </button>
      <button
        onClick={() => finish(true)}
        disabled={busy}
        className="w-full rounded-md border border-edge py-2 text-sm font-medium text-ink hover:bg-fog-800"
      >
        Skip
      </button>
    </div>
  );
}
