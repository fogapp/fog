import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { post, type AppDetail, type DeployStatusResponse } from "../api";
import { DeployKey, WebhookURL } from "./GitAccess";
import CancelDeploy from "./CancelDeploy";
import { isHTTPSRepo } from "../lib/gitUrl";

// SetupPanel owns a git app's first run: connect the repo, optionally wire
// the webhook, deploy — with live progress and plain-English failures. Shown
// until the first deploy succeeds; afterwards the same credentials live
// under Config → Source.
export default function SetupPanel({
  name, detail, deployStatus, onDeployStarted, onRemove,
}: {
  name: string;
  detail: AppDetail;
  deployStatus?: DeployStatusResponse;
  onDeployStarted: () => void;
  onRemove: () => void;
}) {
  const [showLog, setShowLog] = useState(false);
  const deploy = useMutation({
    mutationFn: () => post(`/api/apps/${name}/actions`, { action: "deploy" }),
    onSuccess: onDeployStarted,
  });

  const current = deployStatus?.current;
  const last = deployStatus?.last;
  const deploying = current?.running ?? false;
  const failed = !deploying && last?.status === "failed";
  const keyless = isHTTPSRepo(detail.spec.source.repo);

  return (
    <div className="mx-auto max-w-2xl space-y-6">
      <div className="rounded-lg border border-veil-edge bg-veil p-5">
        <h2 className="text-lg font-semibold text-ink">Almost there — let's connect the repository</h2>
        <p className="mt-1 text-sm text-ink-muted">
          {keyless
            ? "Fog builds and runs the app from its public repository — check it's reachable, then deploy."
            : "Two steps: give the repository Fog's key, then deploy. Fog builds and runs the app from source."}
        </p>
      </div>

      {keyless ? (
        <Step n={1} title="Check the repository">
          <p className="mb-2 text-sm text-ink-muted">
            This is a public repository, cloned over https — there's no key to add. Test that Fog can reach it, or go straight to deploying.
          </p>
          {detail.git && <DeployKey name={name} git={detail.git} keyless />}
        </Step>
      ) : (
        <Step n={1} title="Add the deploy key to the repository">
          <p className="mb-2 text-sm text-ink-muted">
            In your git host, open the repository's settings and add this as a <strong className="text-ink-muted">read-only deploy key</strong>.
          </p>
          {detail.git && <DeployKey name={name} git={detail.git} />}
        </Step>
      )}

      <Step n={2} title="Auto-deploy on push (optional)">
        <p className="mb-2 text-sm text-ink-muted">
          Add this as a webhook in the repository (trigger: push) and every push to the branch deploys itself. Skippable — you can always deploy from here.
        </p>
        {detail.git && <WebhookURL git={detail.git} />}
      </Step>

      <Step n={3} title="Deploy">
        {!deploying && (
          <div className="flex flex-wrap items-center gap-2">
            <button onClick={() => deploy.mutate()} disabled={deploy.isPending}
              className="rounded-md bg-accent px-5 py-2.5 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
              {failed ? "Retry deploy" : `Deploy ${name}`}
            </button>
            <button onClick={onRemove} disabled={deploy.isPending}
              className="rounded-md px-4 py-2.5 text-sm text-ink-muted hover:bg-hover hover:text-ink disabled:opacity-50">
              Remove {name}…
            </button>
          </div>
        )}

        {deploying && current && (
          <div className="space-y-2">
            <div className="flex items-center justify-between gap-3">
              <p className="flex items-center gap-2 text-sm text-ink">
                <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-fog-300 border-t-transparent" />
                {current.stage || "working"}…
              </p>
              <CancelDeploy name={name} stage={current.stage} />
            </div>
            <pre className="max-h-64 overflow-y-auto rounded-lg border border-hair bg-code p-3 text-xs text-ink-muted">
              {current.log || "starting…"}
            </pre>
          </div>
        )}

        {failed && last && (
          <div className="mt-3 space-y-2 rounded-lg border border-warn/30 bg-warn/10 p-4">
            <p className="font-medium text-warn">
              {last.diagnosis ? last.diagnosis.title : "The deploy failed"}
            </p>
            <p className="text-sm text-warn">
              {last.diagnosis ? last.diagnosis.fix : "The log below has the details."}
            </p>
            <button onClick={() => setShowLog(!showLog)} className="text-xs text-warn underline">
              {showLog ? "hide log" : "show full log"}
            </button>
            {showLog && (
              <pre className="max-h-64 overflow-y-auto rounded border border-hair bg-code p-2 text-xs text-ink-muted">{last.logTail}</pre>
            )}
          </div>
        )}
      </Step>
    </div>
  );
}

function Step({ n, title, children }: { n: number; title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-hair p-5">
      <h3 className="mb-3 flex items-center gap-2 text-sm font-semibold text-ink">
        <span className="flex h-6 w-6 items-center justify-center rounded-full bg-fog-700 text-xs">{n}</span>
        {title}
      </h3>
      {children}
    </div>
  );
}
