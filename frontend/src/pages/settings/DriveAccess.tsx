// Settings › Storage › Drive, the access half: mount Drive in Finder or Explorer over
// WebDAV. One app password per device, shown once, revocable on its own —
// the MCP-token discipline, because a laptop should never hold the
// operator's own session. The list is the section: the address sits in the
// header beside the "+" that mints another, and the how-to folds away
// under the list for the once-per-machine read.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, get, post, type FilesAccessToken } from "../../api";
import { LoadError, Loading, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import CopyButton from "../../components/CopyButton";
import Field from "../../components/Field";
import { Section } from "../../components/Section";
import { timeAgo } from "../../lib/format";

const KEY = ["files", "access-tokens"] as const;

export default function DriveAccess() {
  const [ask, dialog] = useConfirm();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const [minted, setMinted] = useState<{ name: string; password: string; url: string } | null>(null);
  const [error, setError] = useState("");
  const [howTo, setHowTo] = useState(false);
  const tokens = useQuery({ queryKey: KEY, queryFn: () => get<{ tokens: FilesAccessToken[]; url: string }>("/api/files/access-tokens") });

  const create = useMutation({
    mutationFn: () => post<{ name: string; password: string; url: string }>("/api/files/access-tokens", { name: name.trim() }),
    onSuccess: async (res) => {
      // The row lands before the password is shown, so the two never
      // disagree about whether the device exists.
      await qc.invalidateQueries({ queryKey: KEY });
      setMinted(res);
      setName("");
      setAdding(false);
      setError("");
    },
    onError: (e) => setError(errorText(e, "Could not create the password")),
  });
  const revoke = useMutation({
    mutationFn: (id: number) => del(`/api/files/access-tokens/${id}`),
    onSuccess: () => { setError(""); return qc.invalidateQueries({ queryKey: KEY }); },
    onError: (e) => setError(errorText(e, "Could not revoke")),
  });

  const url = tokens.data?.url ?? "";
  const list = tokens.data?.tokens ?? [];
  const phase = phaseOf(tokens, list.length);

  return (
    <Section
      id="access"
      title="Devices"
      actions={
        <>
          {url && (
            <span className="flex items-center gap-1 font-mono text-xs text-ink-muted">
              {url}
              <CopyButton text={url} />
            </span>
          )}
          {!adding && (
            <Button size="sm" onClick={() => { setAdding(true); setMinted(null); setError(""); }}>New app password</Button>
          )}
        </>
      }
    >
      {phase === "loading" && <Loading label="Loading app passwords…" />}
      {phase === "error" && (
        <LoadError label="Couldn't load the app passwords." error={tokens.error} onRetry={() => tokens.refetch()} />
      )}
      {phase === "empty" && !adding && (
        <p className="text-sm text-ink-muted">No app passwords — nothing can mount Drive as a drive yet.</p>
      )}
      {list.length > 0 && (
        <ul className="divide-y divide-divider/60 rounded-lg border border-veil-edge bg-veil">
          {list.map((t) => (
            <li key={t.id} className="flex items-center gap-3 px-4 py-3">
              <div className="min-w-0 flex-1">
                <p className="text-sm font-medium text-ink">{t.name}</p>
                <p className="text-xs text-ink-muted">
                  Created {timeAgo(t.createdAt)} · {t.lastUsedAt ? `used ${timeAgo(t.lastUsedAt)}` : "never used"}
                </p>
              </div>
              <Button
                size="sm"
                variant="danger"
                onClick={async () => {
                  if (await ask({ title: `Revoke the app password for ${t.name}?`, body: "Anything mounted with it disconnects.", action: "Revoke", danger: true })) revoke.mutate(t.id);
                }}
                disabled={revoke.isPending}
              >
                {revoke.isPending && revoke.variables === t.id ? "Revoking…" : "Revoke"}
              </Button>
            </li>
          ))}
        </ul>
      )}

      {adding && (
        <form
          className="space-y-4 rounded-lg border border-veil-edge bg-veil p-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim()) create.mutate();
          }}
        >
          <p className="text-xs text-ink-muted">
            One password per device, shown once. Revoking it later disconnects only that device.
          </p>
          <div className="max-w-sm">
            <Field label="Device name" autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="laptop" />
          </div>
          <div className="flex items-center gap-3">
            <Button type="submit" variant="primary" disabled={create.isPending || !name.trim()}>
              {create.isPending ? "Creating…" : "Create password"}
            </Button>
            <Button type="button" onClick={() => { setAdding(false); setName(""); setError(""); }}>Cancel</Button>
          </div>
        </form>
      )}

      {minted && (
        <div className="space-y-2 rounded-lg border border-accent/30 bg-accent/10 p-4 text-sm">
          <p className="text-accent">Password for <strong>{minted.name}</strong> — shown once, so copy it now.</p>
          <p className="flex items-center gap-2 font-mono text-xs text-ink">
            <span className="break-all">{minted.password}</span>
            <CopyButton text={minted.password} />
          </p>
          <p className="text-xs text-ink-muted">Server: <code className="font-mono">{minted.url}</code> · username: anything</p>
          <Button variant="ghost" size="sm" onClick={() => setMinted(null)}>Dismiss</Button>
        </div>
      )}

      {error && <p className="text-sm text-warn">{error}</p>}
      {dialog}

      <div className="space-y-2 text-xs text-ink-muted">
        <Button variant="ghost" size="sm" onClick={() => setHowTo(!howTo)} aria-expanded={howTo}>
          {howTo ? "Hide how to connect" : "How to connect"}
        </Button>
        {howTo && (
          <>
            <ul className="list-disc space-y-1 pl-5 text-ink-muted">
              <li><span className="text-ink">macOS</span> — Finder › Go › Connect to Server (⌘K), paste the address, any name, the app password.</li>
              <li><span className="text-ink">Windows</span> — Explorer › This PC › Map network drive › Connect to a Web site, paste the address.</li>
              <li><span className="text-ink">iOS / Android</span> — the Files app (Connect to Server) or any WebDAV-capable app.</li>
            </ul>
            <p>
              Deleting from the mount moves to the Trash, like the page does. Over plain http on the LAN the password travels in the clear; use the https address when the server has one.
            </p>
          </>
        )}
      </div>
    </Section>
  );
}
