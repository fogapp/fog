import { useRef, useState } from "react";
import { postForm } from "../../src/api";
import Button from "../../src/components/Button";
import Field from "../../src/components/Field";
import { Spinner, errorText } from "../../src/components/Async";
import type { KitInfo } from "../types";

export function RestoreScreen({
  kit,
  password,
  setPassword,
  onLoaded,
  onContinue,
  onBack,
}: {
  kit: KitInfo | null;
  password: string;
  setPassword: (v: string) => void;
  onLoaded: (info: KitInfo) => void;
  onContinue: () => void;
  onBack: () => void;
}) {
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);

  const verify = async () => {
    const file = fileRef.current?.files?.[0];
    if (!file) {
      setError("Choose your recovery kit file (fog-recovery-kit-….enc).");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const form = new FormData();
      form.append("kit", file);
      form.append("passphrase", passphrase);
      const res = await postForm<{ kit: KitInfo }>("/api/setup/kit", form);
      onLoaded(res.kit);
    } catch (err) {
      setError(errorText(err, "The kit could not be verified."));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">Restore from a backup</h2>
        <p className="mt-1 text-sm text-ink-muted">
          Bring a previous Fog server back from its encrypted recovery kit. The kit is
          verified now, before anything touches this machine&apos;s disk.
        </p>
      </div>
      {kit ? (
        <>
          <div className="rounded-lg border border-accent/30 bg-accent/10 p-4">
            <p className="text-sm font-medium text-accent">Kit verified</p>
            <p className="mt-1 text-xs text-ink-muted">
              Restoring server <strong>{kit.serverName || "(unnamed)"}</strong>
              {kit.adminUser && <> — admin account {kit.adminUser}</>}. You&apos;ll sign in with
              that server&apos;s existing password. Apps are re-restored from their own backups
              afterwards, from the dashboard.
            </p>
          </div>
          {kit.adminUser && (
            <Field
              label={`Password for ${kit.adminUser} (optional)`}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              hint="The password you sign in with. It also restores console/SSH access on the new machine — leave blank to skip that."
            />
          )}
        </>
      ) : (
        <>
          <label className="block">
            <span className="mb-1 block text-sm font-medium text-ink">Recovery kit</span>
            <input
              ref={fileRef}
              type="file"
              accept=".enc"
              className="w-full text-sm text-ink-muted file:mr-3 file:rounded-md file:border-0 file:bg-fog-700 file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-accent-on hover:file:bg-accent"
            />
          </label>
          <Field
            label="Kit passphrase"
            type="password"
            value={passphrase}
            onChange={(e) => setPassphrase(e.target.value)}
          />
        </>
      )}
      {error && <p className="text-sm text-warn">{error}</p>}
      <div className="flex gap-3">
        <Button className="flex-1" onClick={onBack} disabled={busy}>
          Back
        </Button>
        {kit ? (
          <Button variant="primary" className="flex-1" onClick={onContinue}>
            Continue
          </Button>
        ) : (
          <Button variant="primary" className="flex-1" onClick={verify} disabled={busy}>
            {busy ? <Spinner /> : "Verify kit"}
          </Button>
        )}
      </div>
    </div>
  );
}
