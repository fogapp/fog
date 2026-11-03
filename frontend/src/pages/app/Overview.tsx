// The overview tab's left column: what needs you, and the app in its own
// words — the fog.yaml notes and the markdown in its repo root. Every line
// is a fact fog holds — the standing facts (version, container, Drive,
// federation) live in the right column, and nothing here is a second
// editor. Also the first-deploy panel, which only this tab shows. Fog's
// own activity on the app is its own tab.

import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";
import {
  post,
  type AppContainer, type AppDetail as AppDetailT, type AppStatus, type DeployStatusResponse, type RepoDoc,
} from "../../api";
import { Loading, Spinner } from "../../components/Async";
import Button from "../../components/Button";
import CancelDeploy from "../../components/CancelDeploy";
import Markdown from "../../components/Markdown";
import { formatBytes } from "../../lib/format";
import { paths } from "../../lib/paths";
import type { AppVolume } from "../../lib/useAppData";
import { useAppDoc, useAppDocs, useCatalogEntry } from "../../lib/useAppData";
import type { FilesStatus } from "../../api";
import { Dot, Group, whenShort } from "./cockpit";

// FirstDeploy covers image apps whose install deploy hasn't succeeded yet:
// progress while running, diagnosis + retry or remove on failure.
export function FirstDeploy({ name, detail, deployStatus, deployError, onAction, onRemove }: {
  name: string; detail: AppDetailT; deployStatus?: DeployStatusResponse; deployError: boolean; onAction: () => void;
  onRemove: () => void;
}) {
  const [showLog, setShowLog] = useState(false);
  const deploy = useMutation({
    mutationFn: () => post(`/api/apps/${name}/actions`, { action: "deploy" }),
    onSuccess: onAction,
  });
  const current = deployStatus?.current;
  const last = deployStatus?.last;

  // Nothing known yet: don't offer a Deploy button next to "hasn't been
  // deployed" when the answer is still in flight — and don't spin forever
  // if the answer never comes.
  if (!deployStatus) {
    return deployError ? (
      <p className="text-sm text-warn">Couldn't read this app's deploy status. Reload to try again.</p>
    ) : (
      <Loading label="Checking this app's deploy status…" className="justify-center" />
    );
  }

  // A freshly created image app has its first deploy kicked off server-side,
  // so there's a beat before any deploy record exists. Saying "not deployed
  // yet" there invites a second, redundant deploy.
  const justCreated = Date.now() - new Date(detail.app.createdAt).getTime() < 60_000;
  if (!current?.running && !last && justCreated && detail.spec.source.type === "image") {
    return (
      <div className="mx-auto max-w-2xl rounded-lg border border-hair p-5">
        <Loading label="Starting the first deploy…" />
      </div>
    );
  }

  if (current?.running || last?.status === "running") {
    return (
      <div className="mx-auto max-w-2xl space-y-2 rounded-lg border border-hair p-5">
        <div className="flex items-center justify-between gap-3">
          <p className="flex items-center gap-2 text-sm text-ink">
            <Spinner />
            Installing — {current?.stage || "working"}…
          </p>
          {current?.running && <CancelDeploy name={name} stage={current.stage} />}
        </div>
        <pre className="max-h-64 overflow-y-auto rounded-lg border border-hair bg-code p-3 text-xs text-ink-muted">{current?.log || "starting…"}</pre>
      </div>
    );
  }
  return (
    <div className="mx-auto max-w-2xl space-y-4">
      {last?.status === "failed" ? (
        <div className="space-y-2 rounded-lg border border-warn/30 bg-warn/10 p-5">
          <p className="font-medium text-warn">{last.diagnosis?.title ?? "The install didn't finish"}</p>
          <p className="text-sm text-warn">{last.diagnosis?.fix ?? "The log below has the details."}</p>
          <button onClick={() => setShowLog(!showLog)} className="text-xs text-warn underline">
            {showLog ? "hide log" : "show full log"}
          </button>
          {showLog && <pre className="max-h-64 overflow-y-auto rounded border border-hair bg-code p-2 text-xs text-ink-muted">{last.logTail}</pre>}
        </div>
      ) : (
        <p className="text-sm text-ink-muted">This app hasn't been deployed yet.</p>
      )}
      <div className="flex flex-wrap gap-2">
        <Button variant="primary" onClick={() => deploy.mutate()} disabled={deploy.isPending}>
          {last?.status === "failed" ? "Retry deploy" : "Deploy"}
        </Button>
        <Button variant="ghost" onClick={onRemove} disabled={deploy.isPending}>
          Remove {name}…
        </Button>
      </div>
    </div>
  );
}

// One thing that needs a person, with the fix beside it.
interface Need {
  key: string;
  text: ReactNode;
  actions?: ReactNode;
}

export function Overview({ name, detail, status, container, deployStatus, volumes, filesStatus, restorePending, onGoLogs, onGoDrive, onCancelRestore }: {
  name: string;
  detail: AppDetailT;
  status?: AppStatus;
  container?: AppContainer;
  deployStatus?: DeployStatusResponse;
  volumes: AppVolume[];
  filesStatus?: FilesStatus;
  restorePending?: boolean;
  onGoLogs: () => void;
  onGoDrive: () => void;
  onCancelRestore: () => void;
}) {
  const [showFailLog, setShowFailLog] = useState(false);
  const { entry } = useCatalogEntry(name);
  const notes = detail.app.repoNotes || detail.spec.notes;
  // Only a git app has a checkout to read docs from; asking for an image
  // app's would only ever answer "none".
  const docs = useAppDocs(name, detail.spec.source.type === "git");
  const deploying = deployStatus?.current.running ?? false;
  const lastDeploy = deployStatus?.last;

  // Needs you: each line is a finding fog has actually made. Nothing here
  // is inferred from a request not having answered yet.
  const needs: Need[] = [];
  if (!deploying && lastDeploy?.status === "failed") {
    needs.push({
      key: "deploy",
      text: (
        <>
          The last deploy failed{lastDeploy.diagnosis ? `: ${lastDeploy.diagnosis.title}` : ""}. {lastDeploy.diagnosis?.fix ?? ""} The previous version is still what's running.
          {showFailLog && <pre className="mt-2 max-h-48 overflow-auto rounded border border-hair bg-code p-2 font-mono text-xs text-ink-muted">{lastDeploy.logTail}</pre>}
        </>
      ),
      actions: (
        <>
          <Button size="sm" variant="ghost" onClick={() => setShowFailLog(!showFailLog)}>{showFailLog ? "Hide log" : "Show log"}</Button>
          <Link to={paths.app(name, "history")} className="text-xs text-ink-muted hover:text-ink">Deploy history</Link>
        </>
      ),
    });
  }
  if (restorePending) {
    needs.push({
      key: "restore",
      text: <>A restore is staged in {name}'s volumes. It is either still running, or the app can't consume it and retries on every boot.</>,
      actions: <Button size="sm" onClick={onCancelRestore}>Cancel restore</Button>,
    });
  }
  if (status?.state === "running" && status.healthy === false) {
    needs.push({
      key: "health",
      text: <>The health check {detail.spec.health ? <span className="font-mono">{detail.spec.health.path}</span> : ""} is failing.</>,
      actions: <Button size="sm" onClick={onGoLogs}>Open logs</Button>,
    });
  }
  if (container?.found && !container.running && container.finishedAt && (container.exitCode !== 0 || container.oomKilled)) {
    needs.push({
      key: "exit",
      text: container.oomKilled
        ? <>The container was killed for running out of memory (exit {container.exitCode}) at {whenShort(container.finishedAt)}.</>
        : <>The container exited with code {container.exitCode} at {whenShort(container.finishedAt)}.</>,
      actions: <Button size="sm" onClick={onGoLogs}>Open logs</Button>,
    });
  }
  const fsLow = filesStatus?.totalBytes && filesStatus.freeBytes !== undefined && filesStatus.freeBytes < filesStatus.totalBytes * 0.1;
  const driveVols = volumes.filter((v) => v.rel !== undefined);
  if (fsLow && driveVols.length > 0) {
    needs.push({
      key: "disk",
      text: <>The disk behind Drive has only {formatBytes(filesStatus!.freeBytes!)} free; {driveVols.map((v) => v.volume.mount).join(", ")} {driveVols.length === 1 ? "lives" : "live"} there.</>,
      actions: <Button size="sm" onClick={onGoDrive}>Move…</Button>,
    });
  }
  if (status?.openErrors) {
    needs.push({
      key: "errors",
      text: <>{status.openErrors} open error {status.openErrors === 1 ? "group" : "groups"} reported by the app.</>,
      actions: <Link to={paths.app(name, "errors")} className="text-xs text-ink-muted hover:text-ink">Open errors</Link>,
    });
  }

  return (
    <div>
      {deploying && deployStatus?.current && (
        <Group title="Deploying" first right={<CancelDeploy name={name} stage={deployStatus.current.stage} />}>
          <div className="rounded-lg border border-veil-edge bg-veil px-4 py-3">
            <p className="flex items-center gap-2 text-cell text-ink">
              <Spinner />
              {deployStatus.current.stage || "working"}…
            </p>
            <pre className="mt-2 max-h-40 overflow-y-auto rounded border border-hair bg-code p-2 font-mono text-xs text-ink-muted">{deployStatus.current.log}</pre>
          </div>
        </Group>
      )}

      {needs.length > 0 && (
        <Group title="Needs you" first={!deploying}>
          <div className="flex flex-col gap-2">
            {needs.map((n) => (
              <div key={n.key} className="flex flex-wrap items-center gap-3.5 rounded-lg border border-warn/30 bg-warn/10 px-4 py-[13px]">
                <Dot tone="warn" />
                <div className="min-w-[220px] flex-1 text-cell text-ink">{n.text}</div>
                {n.actions}
              </div>
            ))}
          </div>
        </Group>
      )}

      {(notes || (docs.data?.length ?? 0) > 0) && (
        <Group title="About" first={needs.length === 0 && !deploying} right={entry ? <span className="text-hint text-ink-muted">{entry.category}</span> : undefined}>
          <Docs name={name} notes={notes} fromRepo={!!detail.app.repoNotes} docs={docs.data ?? []} />
        </Group>
      )}
    </div>
  );
}

// The app in its own words. One pill per source — each markdown file in
// the repo root under its humanized name, README first, then the fog.yaml
// notes as "Fog" — and the chosen one rendered below in full. The first
// pill is the default, so the README is what opens. A single source needs
// no pills; it just reads.
function Docs({ name, notes, fromRepo, docs }: { name: string; notes?: string; fromRepo: boolean; docs: RepoDoc[] }) {
  const tabs: { id: string; label: string; file?: string }[] = [
    ...docs.map((d) => ({ id: d.file, label: humanizeDocName(d.file), file: d.file })),
    ...(notes ? [{ id: "fog", label: "Fog" }] : []),
  ];
  const [picked, setPicked] = useState<string | null>(null);
  const active = tabs.find((t) => t.id === picked) ?? tabs[0];
  const doc = useAppDoc(name, active?.file);

  if (!active) return null;
  return (
    <div>
      {tabs.length > 1 && (
        <div className="mb-4 flex flex-wrap gap-1.5">
          {tabs.map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => setPicked(t.id)}
              aria-pressed={t.id === active.id}
              className={`whitespace-nowrap rounded-full border px-2.5 py-[3px] text-xs transition-colors ${
                t.id === active.id ? "border-accent/30 bg-accent/10 text-accent" : "border-veil-edge text-ink-muted hover:text-ink"
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>
      )}
      {!active.file ? (
        <Description key="fog" text={notes ?? ""} hint={fromRepo ? "From fog.yaml in the app's repository." : undefined} />
      ) : !doc.data ? (
        doc.isError
          ? <p className="text-hint text-warn">Couldn't read {active.file} from the app's repository.</p>
          : <Loading label={`Reading ${active.file}…`} />
      ) : (
        <Description
          key={active.file}
          text={doc.data.content}
          hint={`${active.file} in the app's repository.${doc.data.truncated ? " Only the first part is shown." : ""}`}
        />
      )}
    </div>
  );
}

// README.md → "Readme", GETTING_STARTED.md → "Getting started". The file's
// shouting is a git convention, not a title.
function humanizeDocName(file: string): string {
  const base = file.replace(/\.[^.]+$/, "").replace(/[_-]+/g, " ").trim().toLowerCase();
  if (!base) return file;
  return base.charAt(0).toUpperCase() + base.slice(1);
}

// The text in full, with the line that says where it came from.
function Description({ text, hint }: { text: string; hint?: string }) {
  return (
    <div>
      <Markdown>{text}</Markdown>
      {hint && <p className="mt-2 text-xs text-ink-muted">{hint}</p>}
    </div>
  );
}
