// The subheader and the actions that apply to the whole app. One hook owns
// every mutation — start, stop, restart, redeploy, update, check, delete —
// so the masthead, the standing column and the keyboard all drive the same
// state, and a busy label in the subheader is the only place "Restarting…"
// is said. The rarer verbs sit behind More…; the two an operator reaches
// for — Restart (or Start) and Open app — never move.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";
import {
  ApiError, del, post,
  type AppDetail as AppDetailT, type AppStatus, type UpdateCheckResponse,
} from "../../api";
import { Spinner, errorText } from "../../components/Async";
import Button from "../../components/Button";
import { copyText } from "../../components/CopyButton";
import Modal from "../../components/Modal";
import StateBadge from "../../components/StateBadge";
import Subheader, { Note } from "../../components/Subheader";
import type { TabItem } from "../../components/Tabs";
import { paths } from "../../lib/paths";
import type { DeployWait } from "../../lib/useAppData";
import { appCheckVerdict, toneClass, type Verdict } from "../../lib/updates";
import { useSnapshotsBeforeUpdate } from "../../lib/backupPolicy";

export type AppModal = "update" | "rollback" | "uninstall" | "more" | "share" | null;

const BUSY: Record<string, string> = {
  start: "Starting…",
  stop: "Stopping…",
  restart: "Restarting…",
  deploy: "Redeploying…",
};

export function useAppActions({ name, status, statusKnown, deploying, onAction, awaitDeploy }: {
  name: string;
  status?: AppStatus;
  statusKnown: boolean;
  deploying: boolean;
  onAction: () => Promise<unknown>;
  // Resolves when the deploy just started has run its course, watching the
  // page's one deploy-status poll (useAppDetail) rather than a second one.
  awaitDeploy: () => Promise<DeployWait>;
}) {
  const navigate = useNavigate();
  const [error, setError] = useState("");
  const [verdict, setVerdict] = useState<Verdict | null>(null);
  // Which version change is in flight, if any. Update and rollback are the
  // same operation pointed in opposite directions — both replace what is
  // running underneath this page — so they share one piece of state and one
  // wait loop, and the masthead says which one is happening.
  const [versionOp, setVersionOp] = useState<"" | "update" | "rollback">("");
  // update()'s wait can outlast a visit to this page; every await checks
  // this so a navigation away drops the result instead of acting on a page
  // that is gone.
  const gone = useRef(false);
  useEffect(() => {
    gone.current = false;
    return () => { gone.current = true; };
  }, []);

  const action = useMutation({
    mutationFn: (a: string) => post(`/api/apps/${name}/actions`, { action: a }),
    onSuccess: () => {
      setError("");
      return onAction();
    },
    onError: (e) => setError(errorText(e, "Action failed")),
  });
  // The check asks about this app and reports what came back. It waits for
  // the refreshed status too, so the verdict and the update pill can never
  // disagree — a 200 alone isn't grounds for saying "up to date".
  const check = useMutation({
    mutationFn: () => post<UpdateCheckResponse>(`/api/updates/check?app=${encodeURIComponent(name)}`),
    onSuccess: async (res) => {
      setError("");
      await onAction();
      setVerdict(appCheckVerdict(res.results[0]));
    },
    onError: (e) => {
      setVerdict(null);
      setError(errorText(e, "Update check failed"));
    },
  });
  // A delete that keeps the data stops when the app can't confirm revoking
  // what it gave paired households through Connect (409
  // connect_revoke_failed): those credentials live in its data. The
  // uninstall sheet then asks again — delete the data too, or keep it with
  // keepUnrevoked — so the message is held apart from the general error.
  const [revokeFailed, setRevokeFailed] = useState("");
  const remove = useMutation({
    mutationFn: (v: { data: boolean; keepUnrevoked?: boolean }) =>
      del(`/api/apps/${name}?data=${v.data ? "1" : "0"}${v.keepUnrevoked ? "&keepUnrevoked=1" : ""}`),
    onMutate: () => {
      setError("");
      setRevokeFailed("");
    },
    onSuccess: () => navigate(paths.home),
    onError: (e) => {
      if (e instanceof ApiError && e.status === 409 && (e.body as { code?: string } | undefined)?.code === "connect_revoke_failed") {
        setRevokeFailed(e.message);
        return;
      }
      setError(errorText(e, "Delete failed"));
    },
  });

  // Updating rebuilds and restarts the app underneath this page. The
  // masthead says "Updating…" for the whole of it. When the deploy ends,
  // the queries behind this page are refetched (via onAction) so the state
  // shown is the state that resulted.
  // skipSnapshot leaves out the pre-update snapshot for this run only — the
  // way past a snapshot that is itself what keeps failing.
  const update = (skipSnapshot = false) =>
    changeVersion("update", () => post(`/api/apps/${name}/update`, skipSnapshot ? { skipSnapshot } : undefined));

  // Rolling back is the same journey to an older version: fog snapshots (if
  // the app is set to), redeploys the version named, and re-checks — so the
  // update badge comes back up rather than the app quietly reading as
  // current while running something deliberately older.
  const rollback = (ref: string) => changeVersion("rollback", () => post(`/api/apps/${name}/rollback`, { ref }));

  async function changeVersion(kind: "update" | "rollback", start: () => Promise<unknown>) {
    setVersionOp(kind);
    setError("");
    try {
      await start();
    } catch (e) {
      if (gone.current) return;
      setError(errorText(e, kind === "rollback" ? "The rollback didn't start" : "The update didn't start"));
      setVersionOp("");
      return;
    }
    const outcome = await awaitDeploy();
    if (outcome === "gone" || gone.current) return;
    await onAction();
    if (gone.current) return;
    setVersionOp("");
    if (outcome === "timeout") {
      setError(`The ${kind} is still running after 15 minutes — check the deploy log under History.`);
    }
  }

  const running = status?.state === "running";
  const updating = versionOp !== "";
  // Nothing state-dependent is offered until the container's state is known:
  // guessing here means lighting up Start for an app that's already running.
  const ready = statusKnown && !deploying && !action.isPending && !updating;
  const busy = action.isPending
    ? BUSY[action.variables!] ?? "Working…"
    : versionOp === "rollback" ? "Rolling back…"
    : versionOp === "update" ? "Updating…"
    : check.isPending ? "Checking for updates…" : deploying ? "Deploying…" : "";

  return {
    act: (a: "start" | "stop" | "restart" | "deploy") => action.mutate(a),
    acting: action.isPending,
    busy,
    ready,
    running,
    update,
    rollback,
    updating,
    check: () => check.mutate(),
    checking: check.isPending,
    verdict: check.isPending ? null : verdict,
    remove: (data: boolean, keepUnrevoked = false) => remove.mutate({ data, keepUnrevoked }),
    removing: remove.isPending,
    revokeFailed,
    error,
  };
}

export type AppActions = ReturnType<typeof useAppActions>;

// The subheader: one row under the window header, which already names the
// app. Tabs on the left; on the right the state pill, the address, and one
// segmented group of icon buttons — copy the address, open the app,
// restart (or start), more. The verbs are icons because the row is
// compact and the footer legend already spells the keys; every one carries
// its name as a title and label.
export function AppSubheader({ openURL, status, statusKnown, statusError, actions, onMore, stale, items }: {
  openURL: string | null;
  status?: AppStatus;
  statusKnown: boolean;
  statusError: boolean;
  actions: AppActions;
  onMore: () => void;
  // The status poll has started failing while old figures are still on
  // screen: the state pill is real, just not current, and says so.
  stale?: boolean;
  items: readonly TabItem[];
}) {
  const busyLabel = actions.busy || (!statusKnown ? (statusError ? "State unavailable" : "Checking state…") : "");
  // The state pill turns transitional while an action runs: the container
  // is between states, and "Running" beside "Restarting…" would be a lie.
  const shown: AppStatus | undefined =
    status && actions.acting && actions.busy ? { ...status, state: actions.busy.replace("…", "").toLowerCase() } : status;
  const restartable = actions.running || !statusKnown;
  return (
    <Subheader
      label="App sections"
      items={items}
      right={
        <>
          {busyLabel && (
            <span className="inline-flex items-center gap-2 text-hint text-ink-muted">
              {actions.busy && <Spinner />}
              {busyLabel}
            </span>
          )}
          {actions.verdict && <span className={`text-hint ${toneClass[actions.verdict.tone]}`}>{actions.verdict.text}</span>}
          <StateBadge status={shown} known={statusKnown} error={statusError} />
          {openURL && (
            <a href={openURL} target="_blank" rel="noreferrer" className="font-mono text-hint text-ink-muted hover:text-ink">
              {openURL.replace(/^https?:\/\//, "")}
            </a>
          )}
          <span className="inline-flex divide-x divide-hair rounded-md border border-edge">
            <CopyIcon text={openURL} />
            <IconButton
              label="Open app"
              href={openURL ?? undefined}
              disabled={!openURL}
              title={openURL ? "Open app" : statusKnown ? "No web address, or the app isn't running" : "Waiting for the app's state"}
              tone="accent"
            >
              <OpenGlyph />
            </IconButton>
            <IconButton
              label={restartable ? "Restart" : "Start"}
              title={`${restartable ? "Restart" : "Start"} · R`}
              onClick={() => actions.act(restartable ? "restart" : "start")}
              disabled={!actions.ready || (restartable && !actions.running)}
            >
              {actions.acting ? <Spinner size="h-3.5 w-3.5" tone="border-fog-300" /> : restartable ? <RestartGlyph /> : <PlayGlyph />}
            </IconButton>
            <IconButton label="More" title="More…" onClick={onMore} align="end" edge="end">
              <MoreGlyph />
            </IconButton>
          </span>
        </>
      }
      notes={
        <>
          {actions.error && <Note tone="warn">{actions.error}</Note>}
          {stale && <Note>Can't reach fogd — this app's state may be out of date.</Note>}
        </>
      }
    />
  );
}

// One cell of the segmented group. A link when it opens somewhere, a button
// otherwise; disabled cells stay in the row so the group never changes
// shape as the app changes state. The tooltip is fog's own — a quiet veil
// under the cell on hover or focus — rather than the browser's title, which
// arrives late and looks like someone else's.
export function IconButton({ label, title, onClick, href, disabled, tone, align = "center", edge, children }: {
  label: string;
  title?: string;
  onClick?: () => void;
  href?: string;
  disabled?: boolean;
  tone?: "accent";
  align?: "center" | "end";
  // The end cells round with the group's corners; the group cannot clip
  // them itself without clipping the tooltips too.
  edge?: "start" | "end" | "both";
  children: ReactNode;
}) {
  const cls = `inline-flex h-7 w-8 items-center justify-center transition-colors ${
    edge === "start" ? "rounded-l-[5px]" : edge === "end" ? "rounded-r-[5px]" : edge === "both" ? "rounded-[5px]" : ""
  } ${
    disabled
      ? "cursor-not-allowed text-ink-faint"
      : tone === "accent"
        ? "text-accent hover:bg-accent/10"
        : "text-ink-muted hover:bg-hover hover:text-ink"
  }`;
  const control =
    href && !disabled ? (
      <a href={href} target="_blank" rel="noreferrer" aria-label={label} className={cls}>
        {children}
      </a>
    ) : (
      <button type="button" aria-label={label} onClick={onClick} disabled={disabled} className={cls}>
        {children}
      </button>
    );
  return (
    <span className="group relative inline-flex">
      {control}
      <span
        role="tooltip"
        className={`pointer-events-none absolute top-full z-20 mt-1.5 whitespace-nowrap rounded-md border border-veil-edge bg-fog-900 px-2 py-1 text-xs text-ink opacity-0 shadow-lg shadow-black/40 transition-opacity duration-150 group-hover:opacity-100 group-focus-within:opacity-100 ${
          align === "end" ? "right-0" : "left-1/2 -translate-x-1/2"
        }`}
      >
        {title ?? label}
      </span>
    </span>
  );
}

// "Copied" is a claim — the tick shows only after a copy that worked.
function CopyIcon({ text }: { text: string | null }) {
  const [state, setState] = useState<"idle" | "ok" | "fail">("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  return (
    <IconButton
      label="Copy address"
      title={state === "ok" ? "Copied" : state === "fail" ? "Copy failed" : "Copy address"}
      disabled={!text}
      edge="start"
      onClick={async () => {
        if (!text) return;
        const ok = await copyText(text);
        setState(ok ? "ok" : "fail");
        clearTimeout(timer.current);
        timer.current = setTimeout(() => setState("idle"), 1500);
      }}
    >
      {state === "ok" ? <TickGlyph /> : state === "fail" ? <CrossGlyph /> : <CopyGlyph />}
    </IconButton>
  );
}

export const glyph = { width: 15, height: 15, viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", strokeWidth: 1.75, strokeLinecap: "round" as const, strokeLinejoin: "round" as const, "aria-hidden": true };
const CopyGlyph = () => (
  <svg {...glyph}><rect x="9" y="9" width="12" height="12" rx="2" /><path d="M5 15V5a2 2 0 0 1 2-2h10" /></svg>
);
export const OpenGlyph = () => (
  <svg {...glyph}><path d="M14 4h6v6" /><path d="M20 4 10 14" /><path d="M18 13v5a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h5" /></svg>
);
const RestartGlyph = () => (
  <svg {...glyph}><path d="M20 12a8 8 0 1 1-2.3-5.7" /><path d="M20 4v5h-5" /></svg>
);
export const PlayGlyph = () => (
  <svg {...glyph}><path d="M7 4.5v15l12-7.5z" /></svg>
);
export const StopGlyph = () => (
  <svg {...glyph}><rect x="6" y="6" width="12" height="12" rx="1.5" /></svg>
);
const MoreGlyph = () => (
  <svg {...glyph}><circle cx="5" cy="12" r="1" /><circle cx="12" cy="12" r="1" /><circle cx="19" cy="12" r="1" /></svg>
);
const TickGlyph = () => (
  <svg {...glyph} className="text-accent"><path d="m5 12 4 4L19 6" /></svg>
);
const CrossGlyph = () => (
  <svg {...glyph} className="text-warn"><path d="M6 6l12 12M18 6 6 18" /></svg>
);

// More…: the verbs that don't belong on a button. A plain list; each line
// does one thing and closes the sheet.
export function MoreMenu({ name, actions, onClose, onUninstall }: {
  name: string;
  actions: AppActions;
  onClose: () => void;
  onUninstall: () => void;
}) {
  const item = "block w-full rounded-md px-3 py-[9px] text-left text-cell text-ink transition-colors hover:bg-hover hover:text-ink disabled:cursor-not-allowed disabled:opacity-40";
  return (
    <Modal title={name} onClose={onClose} width="max-w-xs">
      <div className="flex flex-col gap-0.5">
        <button type="button" className={item} disabled={!actions.ready} onClick={() => { actions.act("deploy"); onClose(); }}>
          Redeploy
        </button>
        <button type="button" className={item} disabled={actions.checking || actions.updating} onClick={() => { actions.check(); onClose(); }}>
          Check for updates
        </button>
        <button type="button" className={item} disabled={!actions.ready || !actions.running} onClick={() => { actions.act("stop"); onClose(); }}>
          Stop app
        </button>
        <a href={`/api/apps/${encodeURIComponent(name)}/export`} download className={item} onClick={onClose}>
          Export config as a spec
        </a>
        <Link to={paths.app(name, "history")} className={item} onClick={onClose}>Deploy history and revisions</Link>
        <Link to={paths.app(name, "terminal")} className={item} onClick={onClose}>Open a shell in the container</Link>
        <button type="button" className={`${item} text-warn hover:bg-warn/10 hover:text-warn`} onClick={() => { onClose(); onUninstall(); }}>
          Uninstall…
        </button>
      </div>
    </Modal>
  );
}

// Update: what fog will do, in order, before it does it. The snapshot step
// is only listed when the spec asks for one — promising a snapshot the
// scheduler won't take is how a rollback turns out to have nothing to roll
// back to. When it does, the operator can leave it out of this one run: the
// escape hatch for a snapshot that is itself what fails or hangs. The spec
// is untouched, so the next update snapshots again.
export function UpdateModal({ name, detail, status, onClose, onUpdate }: {
  name: string;
  detail: AppDetailT;
  status?: AppStatus;
  onClose: () => void;
  onUpdate: (skipSnapshot: boolean) => void;
}) {
  const [skip, setSkip] = useState(false);
  const configured = useSnapshotsBeforeUpdate(detail.spec);
  const snapshot = configured && !skip;
  const isGit = detail.spec.source.type === "git";
  const to = status?.updateTo ? status.updateTo.slice(0, 12) : "";
  const steps = [
    snapshot && "snapshot the app's data and databases",
    isGit ? "fetch and build the new revision" : `pull the new image${to ? ` · ${to}` : ""} and verify its digest`,
    detail.spec.deployStrategy === "restart" ? "stop, swap, start" : "start the new replica beside the old one",
    detail.spec.health ? `health check ${detail.spec.health.path}` : "wait for the container to stay up",
    detail.spec.deployStrategy === "restart" ? null : "drain and remove the old replica",
  ].filter((s): s is string => !!s);
  return (
    <Modal title={`Update ${name}${to ? ` to ${to}` : ""}`} onClose={onClose}>
      <p className="text-sm leading-relaxed text-ink">
        {snapshot
          ? <>Fog snapshots {name} first, then swaps in the new version. If the new one never comes up, the old one keeps running; restore from Drive if the data needs to go back too.</>
          : skip
            ? <>Fog swaps in the new version without a snapshot first. If the new one never comes up, the old one keeps running — but if the new version changes {name}'s data, the newest snapshot to go back to is the last one taken.</>
            : <>Fog swaps in the new version. If the new one never comes up, the old one keeps running. No snapshot is taken first — {!detail.spec.backup?.enabled ? "this app's backups are off" : detail.spec.backup.beforeUpdate === false ? "this app is set not to snapshot before updates" : "snapshots before updates are off fleet-wide"}.</>}
      </p>
      <div className="mt-3.5 rounded border border-veil-edge bg-code px-3.5 py-3 font-mono text-xs leading-[1.9] text-ink">
        {steps.map((s, i) => <div key={i}>{i + 1} · {s}</div>)}
      </div>
      {configured && (
        <label className="mt-3 flex cursor-pointer items-start gap-2.5 text-cell text-ink">
          <input type="checkbox" checked={skip} onChange={(e) => setSkip(e.target.checked)} className="mt-1" />
          <span>
            Skip the snapshot this time
            <span className="mt-[3px] block text-hint text-ink-muted">For when the snapshot is what keeps failing. Later updates snapshot as usual.</span>
          </span>
        </label>
      )}
      <div className="mt-[18px] flex justify-end gap-2">
        <Button variant="ghost" onClick={onClose}>Cancel</Button>
        <Button variant={skip ? "danger" : "primary"} onClick={() => { onClose(); onUpdate(skip); }}>
          {skip ? "Update without snapshot" : "Update now"}
        </Button>
      </div>
    </Modal>
  );
}

// Uninstall: the container and its config go either way; the choice is
// what happens to the data. Two options, because fog has two: keep the
// app's folders and snapshots, or delete them. The Drive folders a volume
// points at are named in a note of their own, since neither option touches them.
export function UninstallModal({ name, driveFolders, snapshots, actions, onClose, neverInstalled = false }: {
  name: string;
  driveFolders: string[];
  // undefined until the list has loaded — the copy must not say "0 snapshots".
  snapshots?: number;
  actions: AppActions;
  onClose: () => void;
  // An install that never succeeded: same teardown, but it's removing what
  // the attempt left behind, not uninstalling a working app. The data choice
  // stays — a reinstall over kept data can fail too, and that data is real.
  neverInstalled?: boolean;
}) {
  const [choice, setChoice] = useState<"keep" | "delete">("keep");
  const snaps = snapshots === undefined ? "its snapshots" : `${snapshots} snapshot${snapshots === 1 ? "" : "s"}`;
  const opts: { id: "keep" | "delete"; title: string; detail: string }[] = [
    { id: "keep", title: "Keep app data & backups", detail: `Fog-managed data and ${snaps} stay on disk. Reinstalling picks them up.` },
    { id: "delete", title: "Delete everything", detail: `Removes the Fog-managed data and ${snaps}. Cannot be undone.` },
  ];
  return (
    <Modal title={`${neverInstalled ? "Remove" : "Uninstall"} ${name}`} onClose={onClose}>
      <p className="text-sm leading-relaxed text-ink">
        {neverInstalled
          ? "The install didn't finish. Fog removes what the attempt left behind — containers, images, build files and the app's configuration. Choose what happens to any data it created."
          : "The container and its configuration are removed. Choose what happens to the data."}
      </p>
      <div className="mt-3.5 flex flex-col gap-2.5">
        {opts.map((o) => (
          <label key={o.id} className="flex cursor-pointer gap-3 rounded-lg border border-veil-edge bg-veil px-3.5 py-3">
            <input type="radio" name="keep" checked={choice === o.id} onChange={() => setChoice(o.id)} className="mt-1" />
            <span>
              <span className="text-cell text-ink">{o.title}</span>
              <span className="mt-[3px] block text-hint text-ink-muted">{o.detail}</span>
            </span>
          </label>
        ))}
      </div>
      {driveFolders.length > 0 && (
        <p className="mt-2.5 text-hint italic text-ink-muted">
          Drive folders are not affected ({driveFolders.join(", ")}).
        </p>
      )}
      {actions.error && <p className="mt-3 text-hint text-warn">{actions.error}</p>}
      {actions.revokeFailed ? (
        <RevokeFailed message={actions.revokeFailed} actions={actions} onClose={onClose} />
      ) : (
        <div className="mt-[18px] flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={actions.removing}>Cancel</Button>
          <Button variant="danger" onClick={() => actions.remove(choice === "delete")} disabled={actions.removing}>
            {neverInstalled
              ? (actions.removing ? "Removing…" : "Remove")
              : (actions.removing ? "Uninstalling…" : "Uninstall")}
          </Button>
        </div>
      )}
    </Modal>
  );
}

// The app couldn't confirm revoking the access it gave paired households,
// and those credentials live in the data the operator chose to keep. Two
// ways on: delete the data too, which takes the credentials with it, or
// keep it, knowing an app installed later under this name would still
// accept them.
function RevokeFailed({ message, actions, onClose }: { message: string; actions: AppActions; onClose: () => void }) {
  return (
    <div role="alert" className="mt-3.5 rounded-lg border border-warn/40 bg-warn/10 px-3.5 py-3">
      <p className="text-cell text-ink">{message}</p>
      <p className="mt-2 text-hint text-warn">
        Keeping the data anyway leaves those credentials in it: paired households' old access may keep working,
        including against any app you later install under this name.
      </p>
      <div className="mt-3 flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={actions.removing}>Cancel</Button>
        <Button variant="ghost" onClick={() => actions.remove(false, true)} disabled={actions.removing}>
          Keep the data anyway
        </Button>
        <Button variant="danger" onClick={() => actions.remove(true)} disabled={actions.removing}>
          {actions.removing ? "Deleting…" : "Delete its data too"}
        </Button>
      </div>
    </div>
  );
}
