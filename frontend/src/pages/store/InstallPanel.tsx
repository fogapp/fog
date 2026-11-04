// The install card on a listing: the handful of answers Fog needs before
// it can create the app — a name, the inputs nothing can prefill, where
// its folders go — with everything that has a sane default folded under
// Advanced. Companions and a migration artifact ride along. The listing
// around it says what the app is; this card is only how it gets here.

import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { post, postForm, type ArtifactPreview, type CatalogEntry } from "../../api";
import { formatBytes } from "../../lib/format";
import { appsKey, useApps } from "../../lib/useApps";
import AddressField from "../../components/AddressField";
import AppIcon from "../../components/AppIcon";
import Field from "../../components/Field";
import MountPlan from "../../components/MountPlan";
import { errorText } from "../../components/Async";
import { validateGitRepo } from "../../lib/gitUrl";
import { reservedSubdomain, validateSubdomain } from "../../lib/hostname";
import { paths } from "../../lib/paths";
import { buildInstallPayload, defaultValues, fingerprintOf, generateValue, parseDotenv, valuesFromDotenv } from "./install";
import { settingsQuery } from "../../queries";

// The element id the listing's Install button scrolls to.
const INSTALL_PANEL_ID = "install";

export default function InstallPanel({ entry, all }: { entry: CatalogEntry; all: CatalogEntry[] }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone || "Etc/UTC";
  const [name, setName] = useState(entry.id);
  // Empty means "use the app name" — the default, and what gets stored as no
  // host at all. Only a typed value becomes a route host.
  const [subdomain, setSubdomain] = useState("");
  const [values, setValues] = useState<Record<string, string>>(() => defaultValues(entry, browserTZ));
  // Where each folder goes, keyed by mountKey — only the ones the operator
  // has actually chosen. The rest default from the app's name as it is
  // typed, so renaming the app carries its folder along.
  const [placements, setPlacements] = useState<Record<string, string>>({});
  const [showAdvanced, setShowAdvanced] = useState(false);
  // Companions the entry marks default start ticked (Hook's downloader).
  const [checked, setChecked] = useState<Record<string, boolean>>(() =>
    Object.fromEntries((entry.companions ?? []).filter((c) => c.default).map((c) => [c.id, true])));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // Migration state: an artifact attached at install time is staged before the
  // first deploy, so the app's first boot restores instead of running its
  // setup wizard.
  const [artifact, setArtifact] = useState<ArtifactPreview | null>(null);
  const [artifactPath, setArtifactPath] = useState("");
  const [confirmIdentity, setConfirmIdentity] = useState(false);
  const [reading, setReading] = useState(false);
  const [matches, setMatches] = useState<Record<string, boolean>>({});
  const fileRef = useRef<HTMLInputElement>(null);
  const envRef = useRef<HTMLInputElement>(null);
  const [envNote, setEnvNote] = useState("");

  // Only delegated (fog-backup/1) apps can be seeded from an artifact — their
  // own boot guard performs the restore.
  const delegated = entry.spec.backup?.mode === "delegated";
  const secretOption = (envKey: string) => (entry.options ?? []).find((o) => o.target === `secret:${envKey}`);
  const fingerprinted = Object.entries(artifact?.secrets ?? {});

  // Verify each pasted secret against the artifact's fingerprint as it's
  // typed. Re-runs on every keystroke; the digest is cheap and the answer must
  // never lag behind the field it describes.
  useEffect(() => {
    if (fingerprinted.length === 0) {
      setMatches({});
      return;
    }
    let stale = false;
    (async () => {
      const next: Record<string, boolean> = {};
      for (const [envKey, want] of fingerprinted) {
        const opt = secretOption(envKey);
        const val = opt ? (values[opt.key] ?? "").trim() : "";
        next[envKey] = val !== "" && (await fingerprintOf(val)) === want.toLowerCase();
      }
      if (!stale) setMatches(next);
    })();
    return () => {
      stale = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [artifact, values]);

  async function attachArtifact(file: File) {
    setReading(true);
    setError("");
    try {
      const form = new FormData();
      form.append("artifact", file);
      const { path } = await postForm<{ path: string }>("/api/system/backup-uploads", form);
      const preview = await post<ArtifactPreview>("/api/system/backup-artifacts/inspect", { path });
      setArtifactPath(path);
      setArtifact(preview);
      // Blank the generated values for every secret the artifact fingerprints:
      // they have to come from the source instance, and a prefilled field that
      // happens to be wrong is the whole trap this flow exists to close.
      setValues((v) => {
        const next = { ...v };
        for (const envKey of Object.keys(preview.secrets ?? {})) {
          const opt = secretOption(envKey);
          if (opt) next[opt.key] = "";
        }
        return next;
      });
    } catch (err) {
      setError(errorText(err, "Couldn't read that artifact"));
      if (fileRef.current) fileRef.current.value = "";
    }
    setReading(false);
  }

  // The source instance's .env, read here and never uploaded: every option
  // whose variable it sets is filled from it, the fingerprinted secrets
  // included, so a migration is one file instead of a value at a time.
  async function loadDotenv(file: File) {
    const env = parseDotenv(await file.text());
    const { values: next, filled } = valuesFromDotenv(entry, values, env);
    setValues(next);
    const names = (entry.options ?? []).filter((o) => filled.includes(o.key)).map((o) => o.target.replace(/^\w+:/, ""));
    setEnvNote(names.length > 0
      ? `Filled ${names.join(", ")} from ${file.name}.`
      : `Nothing in ${file.name} matches a setting on this app.`);
    if (envRef.current) envRef.current.value = "";
  }

  function detachArtifact() {
    const wasClaimed = fingerprinted.map(([k]) => secretOption(k)).filter(Boolean);
    setArtifact(null);
    setArtifactPath("");
    setConfirmIdentity(false);
    setEnvNote("");
    if (fileRef.current) fileRef.current.value = "";
    // Back to a normal install: regenerate the secrets we blanked, and only
    // those — anything else on the form is the operator's own typing.
    setValues((v) => {
      const next = { ...v };
      for (const o of wasClaimed) {
        if (o!.generate && !next[o!.key]) next[o!.key] = generateValue(o!.generate);
      }
      return next;
    });
  }

  // Everything that must be true before an artifact install can proceed. The
  // server preflights again — this is so the answer arrives on the form.
  // The artifact must come from the software this entry installs — its ID,
  // or a name that software had before a rename (Fin was Manor). What the
  // operator calls this instance in fog is a label and never enters into it.
  const software = [entry.id, ...(entry.formerly ?? [])];
  const wrongSoftware = !!artifact && !software.includes(artifact.app);
  const unmatched = fingerprinted.filter(([k]) => !matches[k]).map(([k]) => k);
  const identityUnconfirmed = !!artifact?.identityHost && !confirmIdentity;
  const restoreBlocked = !!artifact && (wrongSoftware || unmatched.length > 0 || identityUnconfirmed);

  // Companion apps ("also install…") — resolved against the catalog, with
  // already-installed ones surfaced instead of offered again.
  const companions = (entry.companions ?? []).flatMap((c) => {
    const target = all.find((e) => e.id === c.id);
    return target ? [{ blurb: c.blurb, target }] : [];
  });
  // Fetched unconditionally, not just for companions: the address field
  // checks the name it would claim against the ones already served.
  const apps = useApps();
  const settings = useQuery(settingsQuery);
  const baseDomain = settings.data?.base_domain ?? "";
  // Until the installed list arrives we don't know which companions are
  // already there — offering to install a duplicate is the misleading case.
  const installedKnown = !!apps.data;
  const installed = new Set((apps.data ?? []).map((a) => a.name));
  const selected = companions.filter((c) => checked[c.target.id] && !installed.has(c.target.id));

  // Primary inputs are the ones Fog genuinely can't decide (no default, no
  // generator); everything prefilled — and anything the entry marks advanced,
  // meaning the app copes fine when it's left blank — lives under Advanced
  // settings.
  //
  // A fingerprinted secret is rendered inside the artifact panel instead, next
  // to the reason it can't be generated — one input per value, never two.
  const claimed = new Set(fingerprinted.map(([k]) => secretOption(k)?.key).filter(Boolean) as string[]);
  const primary = (entry.options ?? []).filter(
    (o) => !o.default && !o.generate && !o.advanced && o.key !== "tz" &&
      !claimed.has(o.key) && !o.target.startsWith("volume:"),
  );
  const advanced = (entry.options ?? []).filter(
    (o) => !primary.includes(o) && !claimed.has(o.key) && !o.target.startsWith("volume:"),
  );

  // The address the app will answer on, and what stands in the way of it.
  const label = subdomain.trim().toLowerCase();
  const effectiveLabel = label || name.trim();
  // Hosts already served, so a duplicate is caught here rather than becoming
  // an unreachable app and a warning in the issues inbox. Undefined until the
  // list lands — "no conflict" isn't a claim we've earned before then.
  const takenHosts = apps.data && baseDomain
    ? new Set(apps.data.flatMap((a) => a.hosts ?? []).concat(`fog.${baseDomain}`))
    : undefined;
  // Checked against the address the app would actually get, so a clash with
  // the name-derived default is caught too — not just a typed one.
  const clash = effectiveLabel && takenHosts?.has(`${effectiveLabel}.${baseDomain}`)
    ? `${effectiveLabel}.${baseDomain} is already served by another app — the proxy can only route a hostname to one of them.`
    : null;
  const subdomainError =
    validateSubdomain(subdomain) ??
    (label && reservedSubdomain(label) ? `“${label}” is reserved for Fog itself — pick another address.` : null) ??
    clash;

  // What this machine can't do for the app — decided by the server, which
  // refuses the install with the same words if the button is pressed anyway.
  // The listing above the card carries the explanation; here it only holds
  // the button.
  const blocker = entry.blocker ?? "";

  async function install() {
    setBusy(true);
    setError("");
    try {
      const { spec, secrets } = buildInstallPayload(entry, values, placements, name.trim(), subdomain);
      await post("/api/apps", {
        name: name.trim(), spec, secrets,
        ...(artifact ? { restoreFrom: { path: artifactPath, confirmIdentity } } : {}),
      });
    } catch (err) {
      setError(errorText(err, "Install failed"));
      setBusy(false);
      return;
    }
    // Companions are independent installs with sensible defaults; a failure
    // there shouldn't look like the main install failed.
    const failed: string[] = [];
    for (const c of selected) {
      try {
        const { spec, secrets } = buildInstallPayload(c.target, defaultValues(c.target, browserTZ), {}, c.target.id);
        await post("/api/apps", { name: c.target.id, spec, secrets });
      } catch (err) {
        failed.push(`${c.target.title} (${err instanceof Error ? err.message : "install failed"})`);
      }
    }
    await qc.invalidateQueries({ queryKey: appsKey });
    if (failed.length > 0) {
      setError(`${entry.title} was installed, but not every companion: ${failed.join("; ")}. ` +
        "You can install them individually from the app store — then find " + entry.title + " under Apps.");
      setBusy(false);
      return;
    }
    navigate(paths.app(name.trim()));
  }

  return (
    <div id={INSTALL_PANEL_ID} className="scroll-mt-20 space-y-5 rounded-lg border border-veil-edge bg-veil p-5">
      <div>
        <p className="text-sm font-medium text-ink">Set up {entry.title}</p>
        <p className="mt-0.5 text-hint text-ink-muted">
          Backups and update checks are on by default. Everything here is editable later in the app's Config tab.
        </p>
      </div>
      <Field label="Name" value={name} onChange={(e) => setName(e.target.value)}
        hint={label ? "How the app is labelled in Fog." : "Also becomes the address, unless you set one under Advanced."} />
      {primary.map((o) => (
        <div key={o.key}>
          <Field label={o.label + (o.required ? "" : " (optional)")} value={values[o.key] ?? ""}
            type={o.target.startsWith("secret:") ? "password" : "text"}
            onChange={(e) => setValues({ ...values, [o.key]: e.target.value })} hint={o.hint} />
          {o.target === "repo" && validateGitRepo(values[o.key] ?? "") && (
            <p className="mt-1 text-xs text-warn">{validateGitRepo(values[o.key] ?? "")}</p>
          )}
        </div>
      ))}
      {/* Folders are decided, not asked: the card itemizes every one the
          app will mount, says which half of Drive it is in and what the
          app sees it as, and Change is there for a library that already
          exists somewhere else. */}
      <MountPlan entry={entry} app={name.trim()} placements={placements} onChange={setPlacements} />

      <div className="rounded-lg border border-hair">
        <button type="button" onClick={() => setShowAdvanced(!showAdvanced)}
          className="flex w-full items-center justify-between px-4 py-3 text-sm text-ink-muted hover:text-ink">
          <span>{showAdvanced ? "▾" : "▸"} Advanced settings</span>
          <span className="text-xs text-ink-muted">sane defaults provided</span>
        </button>
        {showAdvanced && (
          <div className="space-y-4 border-t border-hair p-4">
            <AddressField
              value={subdomain} onChange={setSubdomain} placeholder={name.trim() || entry.id}
              baseDomain={baseDomain} loaded={!!settings.data} error={subdomainError} />
            {advanced.map((o) => (
              <div key={o.key}>
                {/* Generated secrets stay legible — they're meant to be copied
                    out; a pasted credential is masked like any other. */}
                <Field label={o.label + (o.required ? "" : " (optional)")} value={values[o.key] ?? ""}
                  type={o.target.startsWith("secret:") && !o.generate ? "password" : "text"}
                  onChange={(e) => setValues({ ...values, [o.key]: e.target.value })} hint={o.hint} />
                {o.target === "repo" && validateGitRepo(values[o.key] ?? "") && (
                  <p className="mt-1 text-xs text-warn">{validateGitRepo(values[o.key] ?? "")}</p>
                )}
                {o.generate && (
                  <button type="button" onClick={() => setValues({ ...values, [o.key]: generateValue(o.generate!) })}
                    className="mt-1 text-xs text-ink-muted hover:text-ink">
                    ↻ regenerate
                  </button>
                )}
              </div>
            ))}
            {advanced.some((o) => o.generate) && (
              <p className="text-xs text-ink-muted">
                Generated values are stored encrypted in Fog's vault and covered by the recovery kit.
              </p>
            )}
          </div>
        )}
      </div>
      {delegated && (
        <div className="space-y-3 rounded-lg border border-hair p-4">
          <div>
            <p className="text-sm font-semibold text-ink">Migrating from another instance?</p>
            <p className="mt-0.5 text-xs text-ink-muted">
              Upload a .fogpack.tar produced by another {entry.title} and Fog stages it before the
              first deploy — the app's first boot restores it instead of running the setup wizard.
            </p>
          </div>
          {!artifact ? (
            <input ref={fileRef} type="file" accept=".tar,application/x-tar" disabled={reading || busy}
              onChange={(e) => {
                const file = e.target.files?.[0];
                if (file) attachArtifact(file);
              }}
              className="text-sm text-ink-muted file:mr-3 file:rounded file:border file:border-edge file:bg-fog-800 file:px-3 file:py-1.5 file:text-sm file:text-ink" />
          ) : (
            <div className="space-y-3">
              <div className="rounded-md border border-edge bg-veil p-3 text-xs text-ink-muted">
                <p className="text-sm text-ink">
                  {artifact.app} artifact · {formatBytes(artifact.totalBytes)}
                </p>
                <p className="mt-1">
                  {artifact.createdAt ? `Produced ${new Date(artifact.createdAt).toLocaleString()}` : "Produced (date unstated)"}
                  {artifact.gitSha && ` from ${artifact.gitSha.slice(0, 12)}`}
                </p>
                {artifact.excluded?.length > 0 && (
                  <p className="mt-1">Not included: {artifact.excluded.join(", ")} — recreated empty on restore.</p>
                )}
                <button type="button" onClick={detachArtifact} className="mt-2 text-xs text-ink-muted hover:text-ink">
                  remove artifact
                </button>
              </div>

              {wrongSoftware && (
                <p className="text-xs text-warn">
                  This artifact was made by “{artifact.app}”, not {entry.title} — it can only be restored
                  into the software that made it.
                </p>
              )}

              {fingerprinted.length > 0 && (
                <div className="space-y-3">
                  <p className="text-xs text-ink-muted">
                    This artifact's data was encrypted with the values below. Copy them from the
                    instance you're migrating from — generating new ones would restore rows that
                    decrypt to nothing, so Fog checks each against the artifact before installing.
                  </p>
                  <div>
                    <button type="button" onClick={() => envRef.current?.click()}
                      className="rounded border border-edge bg-fog-800 px-3 py-1.5 text-sm text-ink hover:text-ink">
                      Fill from a .env file…
                    </button>
                    <input ref={envRef} type="file" className="hidden"
                      onChange={(e) => {
                        const file = e.target.files?.[0];
                        if (file) loadDotenv(file);
                      }} />
                    <p className="mt-1 text-xs text-ink-muted">
                      {envNote || "The source instance's .env — read in your browser, never uploaded."}
                    </p>
                  </div>
                  {fingerprinted.map(([envKey]) => {
                    const opt = secretOption(envKey);
                    if (!opt) {
                      return (
                        <p key={envKey} className="text-xs text-warn">
                          {envKey} is fingerprinted by the artifact but isn't an input on this app —
                          set it in the app's Config tab before its first deploy.
                        </p>
                      );
                    }
                    const val = values[opt.key] ?? "";
                    const ok = matches[envKey];
                    return (
                      <div key={envKey}>
                        <Field label={envKey} type="password" value={val} autoComplete="off"
                          onChange={(e) => setValues({ ...values, [opt.key]: e.target.value })}
                          hint={opt.hint || "From the source instance's environment."} />
                        <p className={`mt-1 text-xs ${ok ? "text-accent" : val ? "text-warn" : "text-ink-muted"}`}>
                          {ok ? "✓ matches the artifact" : val ? "✗ doesn't match the artifact" : "waiting for a value"}
                        </p>
                      </div>
                    );
                  })}
                </div>
              )}

              {artifact.identityHost && (
                <label className="flex items-start gap-2 text-xs text-ink-muted">
                  <input type="checkbox" className="mt-0.5" checked={confirmIdentity}
                    onChange={(e) => setConfirmIdentity(e.target.checked)} />
                  <span>
                    This artifact was produced for host “{artifact.identityHost}”. Restore it here anyway.
                  </span>
                </label>
              )}
            </div>
          )}
          {reading && <p className="text-xs text-ink-muted">Reading artifact…</p>}
        </div>
      )}
      {companions.length > 0 && (
        <div className="space-y-3 rounded-lg border border-hair p-4">
          <div>
            <p className="text-sm font-semibold text-ink">Works well with</p>
            <p className="mt-0.5 text-xs text-ink-muted">
              Optionally install these alongside {entry.title} — each becomes a normal app with sensible defaults.
            </p>
          </div>
          {companions.map(({ blurb, target }) => {
            const done = installed.has(target.id);
            return (
              <label key={target.id}
                className={`flex items-start gap-3 rounded-md border border-hair p-3 ${done ? "opacity-60" : "cursor-pointer hover:border-fog-600"}`}>
                <input type="checkbox" className="mt-1" disabled={done || busy || !installedKnown}
                  checked={done || !!checked[target.id]}
                  onChange={(e) => setChecked({ ...checked, [target.id]: e.target.checked })} />
                <AppIcon entry={target} size={36} className="mt-0.5" />
                <span className="min-w-0">
                  <span className="block text-sm font-medium text-ink">
                    {target.title}
                    {done ? (
                      <span className="ml-2 text-xs font-normal text-ink-muted">already installed</span>
                    ) : !installedKnown ? (
                      <span className="ml-2 text-xs font-normal text-ink-muted">checking…</span>
                    ) : null}
                  </span>
                  <span className="mt-0.5 block text-xs text-ink-muted">{blurb}</span>
                </span>
              </label>
            );
          })}
        </div>
      )}
      {error && <p className="text-sm text-warn">{error}</p>}
      <button onClick={install} disabled={busy || reading || restoreBlocked || !!subdomainError || !!blocker}
        className="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
        {busy ? (artifact ? "Installing & staging restore…" : "Installing…")
          : artifact ? `Install ${entry.title} from artifact`
          : selected.length > 0 ? `Install ${entry.title} + ${selected.length} more`
          : `Install ${entry.title}`}
      </button>
      {restoreBlocked && (
        <p className="text-center text-xs text-ink-muted">
          {wrongSoftware ? `Choose an artifact made by ${entry.title}.`
            : unmatched.length > 0 ? `Waiting on ${unmatched.join(", ")} to match the artifact.`
            : "Confirm the host change to continue."}
        </p>
      )}
      {/* The address input lives under Advanced, so say why the button is off
          without making the operator open the section to find out. */}
      {!restoreBlocked && subdomainError && (
        <p className="text-center text-xs text-warn">{subdomainError}</p>
      )}
      {!restoreBlocked && !subdomainError && blocker && (
        <p className="text-center text-xs text-ink-muted">
          Fix the processor as described above, then come back — the store re-checks on every visit.
        </p>
      )}
    </div>
  );
}
