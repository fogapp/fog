// Settings › Server › Updates: both meanings of the word in one place. Fog's own
// release channel with the self-update trigger comes first — it is the
// update that changes what everything else runs on — and then the apps:
// which have something newer, what their policy does about it, and the
// buttons to check and apply across the fleet.

import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, put, type AppUpdateStatus, type SelfUpdateStatus, type UpdateCheckResponse } from "../../api";
import Button from "../../components/Button";
import Field from "../../components/Field";
import Modal from "../../components/Modal";
import { Section } from "../../components/Section";
import { LoadError, Loading, StaleNote, errorText, phaseOf } from "../../components/Async";
import { timeAgo } from "../../lib/format";
import { paths } from "../../lib/paths";
import { appsKey, useApps } from "../../lib/useApps";
import { useFlash } from "../../lib/useFlash";
import { usePollUntilRestart } from "../../lib/usePollUntilRestart";
import { SettingsPage } from "./Frame";
import { useSettingsForm } from "./useSettingsForm";
import { settingsQuery } from "../../queries";

export default function Updates() {
  return (
    <SettingsPage>
      <FogSection />
      <AppsSection />
    </SettingsPage>
  );
}

const POLICY_LABEL: Record<AppUpdateStatus["policy"], string> = {
  manual: "manual",
  notify: "notify",
  auto: "auto",
};

const updatesStatusKey = ["updates-status"] as const;

// The fleet table: stored facts from the last check, with the check itself
// and the apply as the two verbs. Rows with an update sort first. Deploying
// state comes from the same status poll the sidebar uses, so a row that is
// mid-update greys out here the moment it does there.
function AppsSection() {
  const qc = useQueryClient();
  const [note, flash] = useFlash(6000);
  const status = useQuery({
    queryKey: updatesStatusKey,
    queryFn: () => get<{ apps: AppUpdateStatus[] }>("/api/updates/status"),
    refetchInterval: 30_000,
  });
  const live = useApps({ stats: true, refetchInterval: 5000 });
  const deploying = (name: string) => live.data?.find((a) => a.name === name)?.deploying ?? false;

  const refresh = () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: updatesStatusKey }),
      qc.invalidateQueries({ queryKey: appsKey }),
    ]);

  // The check reports what it found, verbatim: a 200 with three failures
  // is not "everything is up to date".
  const checkAll = useMutation({
    mutationFn: () => post<UpdateCheckResponse>("/api/updates/check"),
    onSuccess: async (res) => {
      await refresh();
      flash(
        res.failed > 0
          ? `Checked ${res.checked} — ${res.updatesAvailable} with updates, ${res.failed} couldn't be checked`
          : res.updatesAvailable > 0
            ? `Checked ${res.checked} — ${res.updatesAvailable} with updates`
            : `Checked ${res.checked} — all up to date`,
      );
    },
    onError: (e) => flash(errorText(e, "Update check failed")),
  });
  const update = useMutation({
    mutationFn: (name: string) => post(`/api/apps/${name}/update`),
    onSuccess: refresh,
    onError: (e) => flash(errorText(e, "Update failed")),
  });
  // One at a time, in order, and the count reports what actually started —
  // the engine rejects an update on an app that is already deploying.
  const [applyingAll, setApplyingAll] = useState(false);
  async function updateAll(names: string[]) {
    setApplyingAll(true);
    let started = 0;
    const failed: string[] = [];
    for (const name of names) {
      try {
        await post(`/api/apps/${name}/update`);
        started++;
      } catch (e) {
        failed.push(`${name}: ${errorText(e, "failed")}`);
      }
    }
    await refresh();
    setApplyingAll(false);
    flash(failed.length > 0 ? `Started ${started} of ${names.length} — ${failed.join("; ")}` : `Started ${started} update${started === 1 ? "" : "s"}`);
  }

  const rows = [...(status.data?.apps ?? [])].sort((a, b) => Number(b.updateAvailable) - Number(a.updateAvailable));
  const withUpdate = rows.filter((r) => r.updateAvailable && !deploying(r.app)).map((r) => r.app);
  const phase = phaseOf(status, status.data?.apps.length);
  const checkedAt = rows.map((r) => r.checkedAt).filter(Boolean).sort().at(-1);

  return (
    <Section
      id="apps"
      title="Apps"
      description={
        !status.data
          ? undefined
          : withUpdate.length > 0
            ? `${withUpdate.length} app${withUpdate.length === 1 ? " has" : "s have"} something newer.`
            : checkedAt
              ? `Nothing newer as of the last check, ${timeAgo(checkedAt)}.`
              : "No check has run yet."
      }
      actions={
        <>
          {note && <span className="text-xs text-ink-muted">{note}</span>}
          <Button onClick={() => checkAll.mutate()} disabled={checkAll.isPending || applyingAll}>
            {checkAll.isPending ? "Checking…" : "Check all now"}
          </Button>
          {withUpdate.length > 1 && (
            <Button variant="info" onClick={() => updateAll(withUpdate)} disabled={applyingAll || update.isPending}>
              {applyingAll ? "Starting…" : `Update all (${withUpdate.length})`}
            </Button>
          )}
        </>
      }
    >
      {phase === "loading" ? (
        <Loading label="Reading update status…" />
      ) : phase === "error" ? (
        <LoadError label="Couldn't read update status." error={status.error} onRetry={() => status.refetch()} />
      ) : phase === "empty" ? (
        <p className="text-sm text-ink-muted">No apps yet.</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border border-veil-edge bg-veil">
          <table className="w-full text-left text-cell">
            <thead>
              <tr className="caps-label border-b border-hair-strong">
                <th className="px-4 py-2.5 font-medium">App</th>
                <th className="px-3 py-2.5 font-medium">Status</th>
                <th className="hidden px-3 py-2.5 font-medium md:table-cell">Running</th>
                <th className="hidden px-3 py-2.5 font-medium md:table-cell">Available</th>
                <th className="px-3 py-2.5 font-medium">Policy</th>
                <th className="hidden px-3 py-2.5 font-medium lg:table-cell">Checked</th>
                <th className="px-3 py-2.5" />
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => {
                const busy = (update.isPending && update.variables === r.app) || deploying(r.app);
                return (
                  <tr key={r.app} className="border-b border-divider last:border-0">
                    <td className="px-4 py-2.5">
                      <Link to={paths.app(r.app)} className="font-medium text-ink hover:underline">{r.app}</Link>
                      <span className="ml-2 text-xs text-ink-muted">{r.source}</span>
                    </td>
                    <td className="px-3 py-2.5 text-xs">
                      {deploying(r.app) ? (
                        <span className="text-accent">deploying…</span>
                      ) : r.updateAvailable ? (
                        <span className="rounded-full bg-accent/10 px-2 py-0.5 font-medium text-accent">update available</span>
                      ) : !r.checkedAt ? (
                        <span className="text-ink-muted">not checked yet</span>
                      ) : !r.comparable ? (
                        <span className="text-ink-muted">nothing to compare yet</span>
                      ) : (
                        <span className="text-accent">up to date</span>
                      )}
                    </td>
                    <td className="hidden px-3 py-2.5 font-mono text-xs text-ink-muted md:table-cell">{r.current || "—"}</td>
                    <td className="hidden px-3 py-2.5 font-mono text-xs text-ink-muted md:table-cell">{r.available || "—"}</td>
                    <td className="px-3 py-2.5 text-xs text-ink-muted">
                      <Link to={paths.app(r.app, "config")} className="hover:underline" title="Edit under Config → Runtime">
                        {POLICY_LABEL[r.policy] ?? r.policy}
                      </Link>
                    </td>
                    <td className="hidden px-3 py-2.5 text-xs text-ink-muted lg:table-cell">{r.checkedAt ? timeAgo(r.checkedAt) : "—"}</td>
                    <td className="px-3 py-2.5 text-right">
                      {r.updateAvailable && (
                        <Button size="sm" variant="info" onClick={() => update.mutate(r.app)} disabled={busy || applyingAll}>
                          {busy ? "…" : "Update"}
                        </Button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {status.isError && status.data && <StaleNote what="this table" />}
    </Section>
  );
}

const selfUpdateKey = ["self-update"] as const;
const secretsKey = ["system-secrets"] as const;

// Fog's own release. The check runs on arrival: the section's whole
// question is "is there something newer", and leaving that behind a
// button meant the answer was never on screen when the page opened. The
// button stays, for asking again after correcting the source or when a
// release has just landed.
function FogSection() {
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [msg, setMsg] = useState("");
  const restart = usePollUntilRestart();
  const updating = restart.waiting;

  const check = useQuery({
    queryKey: selfUpdateKey,
    queryFn: () => get<SelfUpdateStatus>("/api/system/self-update"),
    // Every call is a round trip to the release repository, so a remount
    // or a refocus within the minute answers from the last one instead.
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    retry: false,
  });
  const status = check.data;
  const checking = check.isFetching;
  // A transport failure and a check that came back carrying an error are
  // different answers; neither may read as "up to date".
  const failed = check.isError ? errorText(check.error, "The update check couldn't be completed.") : "";

  // The saved source, so the section says where it is looking. An empty
  // setting is not proof that nothing is configured — FOG_UPDATE_BASE_URL
  // is the other half — so an empty one says nothing rather than "unset".
  const settings = useQuery(settingsQuery);
  const repo = (settings.data?.update_base_url ?? "").trim();

  async function apply() {
    const oldVersion = status?.currentVersion;
    setMsg("");
    try {
      await post("/api/system/self-update");
    } catch (e) {
      setMsg(errorText(e, "Update failed"));
      return;
    }
    // Poll until the server is back on a different version, then reload so
    // the browser picks up the new UI. Past the deadline the page says so —
    // reloading blind would show whatever is running as if it were the update.
    restart.begin({ isUp: (s) => !!s.version && s.version !== oldVersion });
  }

  return (
    <Section
      id="fog"
      title="Fog"
      description={repo ? `Checked against ${repo} when this page opens.` : "Checked against the release repository when this page opens."}
      actions={
        <Button size="sm" onClick={() => setSettingsOpen(true)}>Update source</Button>
      }
    >
      <div className="max-w-lg space-y-4">
        <div className="flex flex-wrap items-center gap-3">
          <Button onClick={() => check.refetch()} disabled={checking || updating}>
            {checking ? "Checking…" : status || failed ? "Check again" : "Check for updates"}
          </Button>
          {checking && <Loading label="asking the release repository…" />}
          {!checking && failed && <span className="text-sm text-warn">{failed}</span>}
          {!checking && !failed && status && !updating && (
            <span className={`text-sm ${status.error ? "text-warn" : "text-accent"}`}>
              {status.error
                ? status.error
                : status.updateAvailable
                  ? `${status.latestVersion} available (running ${status.currentVersion})`
                  : `Up to date (${status.currentVersion})`}
            </span>
          )}
          {status?.updateAvailable && !updating && !checking && (
            <Button variant="primary" onClick={apply}>
              Update &amp; restart
            </Button>
          )}
        </div>
        {updating && (
          <p className="flex items-center gap-2 text-sm text-warn">
            <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-warn/30 border-t-transparent" />
            Updating — fogd is restarting…
          </p>
        )}
        {restart.timedOut && (
          <p className="text-sm text-warn">
            The server hasn't come back on a new version. The update may still be finishing — or it
            failed; check again in a minute.
          </p>
        )}
        {msg && !updating && <p className="text-sm text-warn">{msg}</p>}
        {updating && <p className="text-xs text-ink-muted">This page reloads automatically when the new version is up.</p>}
      </div>
      {settingsOpen && (
        <UpdateSourceModal onClose={() => setSettingsOpen(false)} onSaved={() => void check.refetch()} />
      )}
    </Section>
  );
}

// The source fog checks: which repository, and the token for a private
// one. Both are set once and then left alone, so they sit behind the
// header's settings icon instead of taking the top of the section.
function UpdateSourceModal({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const qc = useQueryClient();
  const { value, set, loading } = useSettingsForm(["update_base_url"]);
  const secrets = useQuery({
    queryKey: secretsKey,
    queryFn: () => get<{ keys: string[] }>("/api/system/secrets"),
  });
  const secretsKnown = !!secrets.data;
  const hasToken = secrets.data?.keys.includes("update_basic_auth") ?? false;
  const [authToken, setAuthToken] = useState("");
  const [error, setError] = useState("");

  const save = useMutation({
    mutationFn: async () => {
      await put("/api/settings", { update_base_url: value("update_base_url").trim() });
      // An empty box means "keep the saved token", never "clear it".
      if (authToken.trim()) {
        await put("/api/system/secrets", { key: "update_basic_auth", value: authToken.trim() });
      }
    },
    onSuccess: async () => {
      await Promise.all([
        qc.invalidateQueries({ queryKey: settingsQuery.queryKey }),
        qc.invalidateQueries({ queryKey: secretsKey }),
      ]);
      // The last answer was about the old source; ask the new one.
      onSaved();
      onClose();
    },
    onError: (e) => setError(errorText(e, "Couldn't save the update source")),
  });

  return (
    <Modal title="Update source" onClose={onClose}>
      <div className="space-y-6">
        {loading ? (
          <Loading label="Loading update settings…" />
        ) : (
          <Field label="Repository URL" value={value("update_base_url")} onChange={set("update_base_url")}
            placeholder="http://gitea.lan/matthew/fog"
            hint="An http:// or https:// URL: a Gitea, Forgejo, GitHub or GitLab repo, a directory of release files, or a latest.json itself. Fog finds the latest signed release there. No spaces, quotes, ?, # or @." />
        )}
        {/* Whether a token is already saved is a fact about the server — the
            hint waits for the answer instead of guessing "none saved". */}
        <Field label="Access token (for private repos)" type="password" value={authToken}
          onChange={(e) => setAuthToken(e.target.value)}
          placeholder={!secretsKnown ? "checking for a saved token…" : hasToken ? "•••••• (saved — enter only to replace)" : "paste the token — nothing else"}
          hint={!secretsKnown
            ? "Checking whether a token is already saved…"
            : hasToken
              ? "A token is saved and will be used — leave this empty unless you want to replace it."
              : "A read-only access token, stored encrypted."} />
        <p className="text-xs text-ink-muted">
          A token is only ever sent to the source it was saved for, and only over https. Moving the source to
          another host deletes it — paste a token for the new host after saving.
        </p>
        {error && <p className="text-sm text-warn">{error}</p>}
        <div className="flex items-center gap-3">
          {/* Disabled until the settings land: saving from an unloaded form
              would write a blank over the stored URL. */}
          <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending || loading}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
          <Button onClick={onClose} disabled={save.isPending}>Cancel</Button>
        </div>
      </div>
    </Modal>
  );
}
