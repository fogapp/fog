// The Updates tab: how this app handles new versions, and where it has
// been. Two questions sit here — "should fog do this for me" and "get me
// off this version" — and both are answered without leaving the tab.
//
// Every row is a deploy fog performed and recorded, never a reading of the
// image tag: a tag moves, so the tag says what is newest, not what this app
// is running. An app whose last deploy predates that record has no rows and
// says so, rather than showing a list that would be a guess.

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { get, post, type AppDetail as AppDetailT, type AppStatus, type AppVersion, type AppVersionsResponse } from "../../api";
import { LoadError, Loading, Spinner, errorText } from "../../components/Async";
import Button from "../../components/Button";
import Modal from "../../components/Modal";
import Toggle from "../../components/Toggle";
import { timeAgo } from "../../lib/format";
import { Group, Head, Pill, Row, Table, Veil, whenShort } from "./cockpit";
import type { AppActions } from "./Actions";
import { appKey, appVersionsKey } from "../../lib/useAppData";
import { useSnapshotsBeforeUpdate } from "../../lib/backupPolicy";

// What the toggle means in each direction. Off is "notify", not "manual":
// an operator turning off *automatic* updates is asking fog to stop acting,
// not to stop looking — a switch that also silenced the news would read as
// broken the first time an update went unmentioned for a month.
const AUTO = "auto";
const OFF = "notify";

export function Updates({ name, detail, status, actions, onUpdate, onRollback }: {
  name: string;
  detail: AppDetailT;
  status?: AppStatus;
  actions: AppActions;
  // Both verbs confirm first, in a dialog that says what fog is about to do
  // — the same one the standing column opens, so "Update" means the same
  // thing wherever it is pressed.
  onUpdate: () => void;
  onRollback: (v: AppVersion) => void;
}) {
  const qc = useQueryClient();
  const snap = useSnapshotsBeforeUpdate(detail.spec);
  const versions = useQuery({
    queryKey: appVersionsKey(name),
    queryFn: () => get<AppVersionsResponse>(`/api/apps/${name}/versions`),
  });

  const policy = useMutation({
    mutationFn: (p: string) => post(`/api/apps/${name}/update-policy`, { policy: p }),
    // The spec carries the policy, so the app query is refetched too — and
    // awaited, so the switch settles into the state the server stored
    // rather than the state it was clicked into.
    onSuccess: () => Promise.all([
      qc.invalidateQueries({ queryKey: appKey(name) }),
      qc.invalidateQueries({ queryKey: appVersionsKey(name) }),
    ]),
  });

  if (!versions.data) {
    return versions.isError
      ? <LoadError label={`Couldn't load ${name}'s versions.`} error={versions.error} onRetry={() => versions.refetch()} />
      : <Loading label="Loading versions…" />;
  }
  const v = versions.data;
  const isGit = v.source === "git";
  const busy = actions.updating || actions.checking || !actions.ready;

  return (
    <div className="max-w-3xl">
      <Group
        first
        title="Updates"
        right={
          <Toggle
            checked={v.policy === AUTO}
            disabled={policy.isPending}
            onChange={(on) => policy.mutate(on ? AUTO : OFF)}
            label="Update automatically"
          />
        }
      >
        <p className="text-hint text-ink-muted">{policyLine(name, v.policy, isGit, detail, snap)}</p>
        {policy.isError && <p className="mt-2 text-hint text-warn">{errorText(policy.error, "Couldn't change the policy")}</p>}
        {v.policy === "manual" && (
          <button
            type="button"
            disabled={policy.isPending}
            onClick={() => policy.mutate(OFF)}
            className="mt-2 text-hint text-accent hover:text-accent-hover disabled:opacity-50"
          >
            Tell me when there's a new version
          </button>
        )}
      </Group>

      <Available name={name} v={v} isGit={isGit} status={status} actions={actions} busy={busy} onUpdate={onUpdate} />

      <Group
        title="Versions"
        right={ <span className="text-hint text-ink-muted">{isGit ? "commits fog built" : "images fog deployed"}</span>}
      >
        {v.versions.length === 0 ? (
          <Veil className="px-4 py-5 text-hint text-ink-muted">
            Fog hasn't recorded a version for {name} yet — its next deploy starts this list, and there is nothing
            to roll back to until then.
          </Veil>
        ) : (
          <Table>
            <Head cols={COLS}>
              <span>When</span>
              <span>{isGit ? "Commit" : "Digest"}</span>
              <span>By</span>
              <span />
            </Head>
            {v.versions.map((row) => (
              <VersionRow key={row.ref} row={row} busy={busy} onRollback={() => onRollback(row)} />
            ))}
          </Table>
        )}
        <p className="mt-3 text-hint text-ink-muted">
          Rolling back redeploys {name} on that {isGit ? "commit" : "image"}, keeping its current configuration.
          {snap && " A snapshot is taken first."}
          {v.policy === AUTO && " Automatic updates are turned off, so the newer version isn't put straight back."}
        </p>
      </Group>
    </div>
  );
}

const COLS = "minmax(0,1fr) 150px 130px auto";

// "9 days ago" is worth saying beside a date; "9/1/2026" is not — timeAgo
// falls back to the locale date once "ago" stops being useful, and printing
// that beside the date it already sits next to says the same thing twice.
function relative(iso: string): string {
  const rel = timeAgo(iso);
  return /ago$|^just now$/.test(rel) ? rel : "";
}

function VersionRow({ row, busy, onRollback }: { row: AppVersion; busy: boolean; onRollback: () => void }) {
  return (
    <Row cols={COLS}>
      <span className="min-w-0 text-ink-muted">
        {whenShort(row.at)}
        {relative(row.at) && <span className="text-ink-faint"> · {relative(row.at)}</span>}
        {row.deploys && row.deploys > 1 ? (
          <span className="text-ink-faint" title={`first deployed ${whenShort(row.firstAt ?? row.at)}`}>
            {" "}· deployed {row.deploys}×
          </span>
        ) : null}
      </span>
      <span className="truncate font-mono text-ink" title={row.image || row.ref}>{row.short}</span>
      <span className="truncate text-ink-muted" title={`config revision ${row.rev}`}>{row.actor || "—"}</span>
      <span className="text-right">
        {row.current ? (
          <Pill tone="ok">Running</Pill>
        ) : (
          <Button size="sm" variant="secondary" disabled={busy} onClick={onRollback}>Roll back</Button>
        )}
      </span>
    </Row>
  );
}

// The band above the list: what the last check found, said as what it
// actually found. A check that couldn't compare anything is its own answer
// — never "up to date" — and the time it ran is on screen beside it, so a
// verdict from last week can't pass for one from this morning.
function Available({ name, v, isGit, status, actions, busy, onUpdate }: {
  name: string; v: AppVersionsResponse; isGit: boolean; status?: AppStatus; actions: AppActions;
  busy: boolean; onUpdate: () => void;
}) {
  const available = v.updateAvailable;
  const checked = v.checkedAt ? `checked ${timeAgo(v.checkedAt)}` : "not checked yet";
  return (
    <Group title="What's newer" right={ <span className="text-hint text-ink-muted">{checked}</span>}>
      <Veil className={`px-4 py-4 ${available ? "border-accent/30 bg-accent/[.07]" : ""}`}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="min-w-0">
            {available ? (
              <p className="text-cell text-accent">
                {v.available ? <><span className="font-mono">{v.available}</span> is available</> : "A new version is available"}
                {isGit ? " on the branch" : " from the registry"}
              </p>
            ) : !v.comparable ? (
              <p className="text-cell text-ink-muted">
                {isGit
                  ? "Nothing to compare yet — this app hasn't been deployed from its repository."
                  : "Nothing to compare yet — this app hasn't been deployed from its image."}
              </p>
            ) : (
              <p className="text-cell text-ink">No newer version — {name} is up to date.</p>
            )}
            <p className="mt-1 text-hint text-ink-muted">
              {v.current
                ? <>Running <span className="font-mono">{shortOf(v)}</span></>
                : "Fog hasn't recorded which version is running — the next deploy records it."}
            </p>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            {actions.checking && <Spinner />}
            <Button size="sm" variant="secondary" disabled={actions.checking || actions.updating} onClick={actions.check}>
              {actions.checking ? "Checking…" : "Check now"}
            </Button>
            {available && (
              <Button size="sm" variant="primary" disabled={busy} onClick={onUpdate}>
                {actions.updating ? "Updating…" : "Update now"}
              </Button>
            )}
          </div>
        </div>
      </Veil>
      {status?.updateAvailable && !available && (
        <p className="mt-2 text-hint text-ink-muted">The app list still shows an update for {name}; check now to settle it.</p>
      )}
    </Group>
  );
}

function shortOf(v: AppVersionsResponse): string {
  const row = v.versions.find((x) => x.current);
  return row?.short ?? v.current?.slice(0, 12) ?? "";
}

// One sentence saying exactly what the stored policy does. "manual" has no
// switch position of its own — it reads as off, and says what it really is,
// because an app that fog checks but never mentions is a state an operator
// should be able to see rather than infer from silence.
function policyLine(name: string, policy: string, isGit: boolean, detail: AppDetailT, snapshots: boolean): string {
  const source = isGit ? `a new commit lands on ${detail.spec.source.branch || "main"}` : "the image tag moves";
  if (policy === "auto") {
    const snap = snapshots ? "snapshots it, " : "";
    return `On — when ${source}, fog ${snap}deploys the new version and checks it comes up healthy. It tells you either way.`;
  }
  if (policy === "notify") {
    return `Off — fog checks, and tells you when ${source}. You decide when ${name} updates.`;
  }
  return `Off — fog checks when ${source}, but says nothing about it. You'll only see it here and on the app's badge.`;
}

// The rollback dialog. It says the one thing the operator cannot see from
// the list: fog puts the old *version* back, not the old data. A newer
// version that migrated this app's database has already changed data the
// old one may not read, and the way back from that is the snapshot — so the
// snapshot is named here, before the button, rather than discovered after.
export function RollbackModal({ name, version, detail, onClose, onRollback }: {
  name: string;
  version: AppVersion;
  detail: AppDetailT;
  onClose: () => void;
  onRollback: (ref: string) => void;
}) {
  const isGit = detail.spec.source.type === "git";
  const snap = useSnapshotsBeforeUpdate(detail.spec);
  // An app on auto would take the newer version straight back at its next
  // check, so the rollback turns auto off — said here, before the button,
  // not discovered when the switch is found flipped.
  const auto = detail.spec.updatePolicy === "auto";
  const steps = [
    snap && "snapshot the app's data and databases",
    isGit ? `check out ${version.short} and build it` : `deploy ${version.short}, pinned by digest`,
    detail.spec.deployStrategy === "restart" ? "stop, swap, start" : "start the old version beside the running one",
    detail.spec.health ? `health check ${detail.spec.health.path}` : "wait for the container to stay up",
    detail.spec.deployStrategy === "restart" ? null : "drain and remove the newer replica",
    auto && "turn automatic updates off, so the newer version isn't put back",
    "check for updates again, so the badge shows what you've stepped back from",
  ].filter((s): s is string => !!s);
  return (
    <Modal title={`Roll ${name} back to ${version.short}`} onClose={onClose}>
      <p className="text-sm leading-relaxed text-ink">
        This version last ran {whenShort(version.at)}. Fog puts it back with {name}'s current configuration — it does
        not put the data back. If a newer version has migrated this app's data since, the old version may not read
        it; {snap
          ? "the snapshot fog takes first is the way out of that, from Drive."
          : "and this app has no snapshot before version changes, so there is no way out of that. Turn snapshots on under Drive first if the data matters."}
      </p>
      {auto && (
        <p className="mt-2 text-sm leading-relaxed text-ink">
          {name} updates automatically, which would put the newer version straight back at the next check — so rolling
          back turns automatic updates off. Turn them on again on the Updates tab once the newer version is fixed.
        </p>
      )}
      <div className="mt-3.5 rounded border border-veil-edge bg-code px-3.5 py-3 font-mono text-xs leading-[1.9] text-ink">
        {steps.map((s, i) => <div key={i}>{i + 1} · {s}</div>)}
      </div>
      <div className="mt-[18px] flex justify-end gap-2">
        <Button variant="ghost" onClick={onClose}>Cancel</Button>
        <Button variant="danger" onClick={() => { onClose(); onRollback(version.ref); }}>
          Roll back
        </Button>
      </div>
    </Modal>
  );
}
