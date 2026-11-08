// A git app's two credentials for talking to its repository: the deploy
// key the host must be given, and the webhook that turns a push into a
// deploy. Shown in the setup panel on first run and under Config → Source
// forever after — one component, so the test and regenerate controls
// behave the same in both places.

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { post, type GitInfo } from "../api";
import CopyButton from "./CopyButton";
import { isHTTPSRepo } from "../lib/gitUrl";
import { appKey } from "../lib/useAppData";

interface TestResult {
  ok: boolean;
  message: string;
  detail?: string;
  fingerprint?: string;
  repoUrl?: string;
  resolvedIps?: string[];
  sshHostKey?: string;
}

// keyless: the app clones a public repository over https, so there is no
// key to hand over — only the connection test is left.
export function DeployKey({ name, git, keyless = false }: { name: string; git: GitInfo; keyless?: boolean }) {
  const qc = useQueryClient();
  const [testResult, setTestResult] = useState<TestResult | null>(null);
  const [showDetail, setShowDetail] = useState(false);

  const test = useMutation({
    mutationFn: () => post<TestResult>(`/api/apps/${name}/test-connection`),
    onSuccess: (r) => {
      setTestResult(r);
      setShowDetail(false);
    },
    onError: (e) => setTestResult({ ok: false, message: e instanceof Error ? e.message : "Test failed" }),
  });
  const regen = useMutation({
    mutationFn: () => post(`/api/apps/${name}/regenerate-deploy-key`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: appKey(name) });
      setTestResult(null);
    },
  });

  return (
    <div className="space-y-3">
      {!keyless && (
        <div className="flex items-start gap-2">
          <code className="block min-w-0 flex-1 overflow-x-auto whitespace-nowrap rounded border border-hair bg-code p-2 text-xs text-ink-muted">
            {git.deployPublicKey}
          </code>
          <CopyButton text={git.deployPublicKey} />
        </div>
      )}
      <div className="flex items-center gap-2">
        <button onClick={() => test.mutate()} disabled={test.isPending}
          className="rounded-md border border-edge px-3 py-1.5 text-sm font-medium text-ink hover:bg-fog-800 disabled:opacity-50">
          {test.isPending ? "Testing…" : "Test connection"}
        </button>
        {!keyless && (
          <button onClick={() => { if (confirm("Generate a new deploy key? The current one stops working and the new one must be added to the repository.")) regen.mutate(); }}
            disabled={regen.isPending}
            className="rounded-md px-2 py-1.5 text-xs text-ink-muted hover:text-ink disabled:opacity-50"
            title="Mint a fresh keypair if the current one is rejected">
            {regen.isPending ? "Regenerating…" : "Regenerate key"}
          </button>
        )}
      </div>
      {testResult && (
        <div className={`rounded-md p-3 text-sm ${testResult.ok ? "bg-accent/10 text-accent" : "bg-warn/10 text-warn"}`}>
          <p>{testResult.ok ? "✓ " : ""}{testResult.message}</p>
          {!testResult.ok && (
            <div className="mt-2 space-y-1 text-xs text-ink-muted">
              {testResult.repoUrl && <p>Tested URL: <code className="text-ink-muted">{testResult.repoUrl}</code></p>}
              {testResult.resolvedIps && testResult.resolvedIps.length > 0 && (
                <p>
                  Host resolves to: <code className="text-ink-muted">{testResult.resolvedIps.join(", ")}</code>
                  {testResult.resolvedIps.length > 1 && <span className="text-warn"> ← multiple addresses!</span>}
                </p>
              )}
              {testResult.sshHostKey && (
                <p>SSH answered by host key: <code className="text-ink-muted">{testResult.sshHostKey}</code></p>
              )}
              {testResult.fingerprint && (
                <p>
                  Fog is presenting key <code className="text-ink-muted">{testResult.fingerprint}</code> — your git host
                  shows each key's fingerprint; they must match exactly.
                </p>
              )}
              {testResult.detail && (
                <>
                  <button onClick={() => setShowDetail(!showDetail)} className="underline">
                    {showDetail ? "hide technical details" : "show technical details"}
                  </button>
                  {showDetail && (
                    <pre className="max-h-40 overflow-y-auto whitespace-pre-wrap rounded border border-hair bg-code p-2 text-ink-muted">{testResult.detail}</pre>
                  )}
                </>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export function WebhookURL({ git }: { git: GitInfo }) {
  const url = window.location.origin + git.webhookPath;
  return (
    <div className="flex items-start gap-2">
      <code className="block min-w-0 flex-1 overflow-x-auto whitespace-nowrap rounded border border-hair bg-code p-2 text-xs text-ink-muted">
        {url}
      </code>
      <CopyButton text={url} />
    </div>
  );
}

// The compact form for the Config document: both credentials with their
// one-line purpose, after the source rows.
export default function GitAccess({ name, git, repo }: { name: string; git: GitInfo; repo?: string }) {
  const keyless = isHTTPSRepo(repo);
  return (
    <div className="mt-3 space-y-4 rounded-lg border border-divider p-4">
      <div className="space-y-2">
        <p className="subhead">
          {keyless
            ? "Public repository — cloned over https, no deploy key needed"
            : "Deploy key — add as a read-only key in your git host"}
        </p>
        <DeployKey name={name} git={git} keyless={keyless} />
      </div>
      <div className="space-y-2">
        <p className="subhead">Webhook — POST on push; deploys immediately (auto policy) or shows the Update button</p>
        <WebhookURL git={git} />
      </div>
    </div>
  );
}
