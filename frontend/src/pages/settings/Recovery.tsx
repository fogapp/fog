// Settings › Storage › Recovery kit: what it takes to rebuild this server
// from nothing. The kit holds what no backup can — the vault master key and
// fog's own database — sealed with a passphrase that has to outlive this
// machine; the manifests export is every app's spec in one file.

import { useQuery } from "@tanstack/react-query";
import Button, { ButtonLink } from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import Field from "../../components/Field";
import { Section } from "../../components/Section";
import { Loading } from "../../components/Async";
import { useAction } from "../../lib/useAction";
import { useSettingToggle } from "../../lib/useSettingToggle";
import { SettingsPage } from "./Frame";
import { useSettingsForm } from "./useSettingsForm";
import { settingsQuery } from "../../queries";

export default function Recovery() {
  return (
    <SettingsPage>
      <KitSection />
      <ExportSection />
    </SettingsPage>
  );
}

// The stored passphrase never comes back from the server — only whether one
// is set — so the box starts empty either way, and an empty box means "leave
// it as it is". Turning the kit off is its own button: clearing a box that
// was never filled can't be what removes the passphrase.
function KitSection() {
  const { value, setValue, SaveButton, loading } = useSettingsForm(["recovery_kit_passphrase"]);
  const settings = useQuery(settingsQuery);
  const remove = useSettingToggle("recovery_kit_passphrase", { errorLabel: "Couldn't remove the passphrase" });
  const isSet = settings.data?.recovery_kit_passphrase_set === "on";
  const [ask, dialog] = useConfirm();
  return (
    <Section
      id="kit"
      title="Passphrase"
      description="The kit holds everything unrecoverable — the vault master key and Fog's database. With a passphrase set, fog writes an encrypted copy beside the nightly backups; keep the passphrase somewhere that survives this machine."
    >
      <div className="max-w-lg space-y-4">
        {loading ? (
          <Loading label="Loading settings…" />
        ) : (
          <Field label="Recovery-kit passphrase" value={value("recovery_kit_passphrase")}
            onChange={(e) => setValue("recovery_kit_passphrase", e.target.value || undefined)}
            placeholder={isSet ? "•••••• (saved — enter only to replace)" : "12+ characters; empty = kit not scheduled"}
            hint={isSet
              ? "A passphrase is saved and the kit is scheduled. Full-server recovery: install fog, upload the kit on the setup screen, then restore each app from its artifact."
              : "Full-server recovery: install fog, upload the kit on the setup screen, then restore each app from its artifact."} />
        )}
        {SaveButton}
        {isSet && (
          <div className="space-y-1">
            <Button variant="ghost" size="sm" disabled={remove.isPending}
              onClick={async () => {
                if (await ask({ title: "Remove the recovery-kit passphrase?", body: "Fog stops writing the nightly kit until you set one again.", action: "Remove passphrase", danger: true })) remove.set("");
              }}>
              {remove.isPending ? "Removing…" : "Remove passphrase"}
            </Button>
            {remove.error && <p className="text-sm text-warn">{remove.error}</p>}
            {dialog}
          </div>
        )}
        <RecoveryKitDownload />
      </div>
    </Section>
  );
}

// The endpoint answers 400 JSON when no recovery-kit passphrase is set — a
// plain <a download> would save that error body to disk as the "kit". Fetch
// first, and turn a refusal into words on this page; a real kit still lands
// as a normal file save.
function RecoveryKitDownload() {
  const { run, busy, error } = useAction();

  async function download() {
    await run(async () => {
      const res = await fetch("/api/system/recovery-kit", { credentials: "same-origin" });
      if (!res.ok) {
        let message = res.statusText;
        try {
          const body = await res.json();
          if (body.error) message = body.error;
        } catch {
          /* non-JSON error body */
        }
        throw new Error(message);
      }
      const blob = await res.blob();
      const cd = res.headers.get("Content-Disposition") ?? "";
      const name = /filename="?([^";]+)"?/.exec(cd)?.[1] ?? "fog-recovery-kit.enc";
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = name;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
    }, "The recovery kit couldn't be generated");
  }

  return (
    <div className="space-y-2 border-t border-hair pt-4">
      <Button onClick={download} disabled={busy}>
        {busy ? "Preparing…" : "Download recovery kit"}
      </Button>
      <p className="text-xs text-ink-muted">Keep a copy off this machine.</p>
      {error && <p className="text-sm text-warn">{error}</p>}
    </div>
  );
}

function ExportSection() {
  return (
    <Section id="export" title="Export" description="Every app's manifest as one file — the specs, not the data — for moving a fleet or keeping a record.">
      <div>
        <ButtonLink href="/api/system/export" download>Export all app manifests</ButtonLink>
      </div>
    </Section>
  );
}
