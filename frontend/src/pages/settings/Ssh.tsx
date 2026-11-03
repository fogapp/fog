// Settings › People & access › SSH: the public keys authorized for root.
// The status line is read from sshd's own effective configuration, so
// "keys work" is only said when sshd would actually accept them.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, get, post, type SshAccessStatus, type SshKey } from "../../api";
import { LoadError, Loading, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import CopyButton from "../../components/CopyButton";
import Field, { TextArea } from "../../components/Field";
import { StatusLine } from "../../components/Section";
import { SettingsPage } from "./Frame";

const KEY = ["system", "ssh"] as const;

export default function Ssh() {
  const [ask, dialog] = useConfirm();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [pub, setPub] = useState("");
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [wireError, setWireError] = useState("");
  const status = useQuery({ queryKey: KEY, queryFn: () => get<SshAccessStatus>("/api/system/ssh") });

  const add = useMutation({
    mutationFn: () =>
      post<{ status: SshAccessStatus; key: SshKey; wireError?: string }>("/api/system/ssh/keys", { key: pub.trim(), name: name.trim() }),
    onSuccess: async (res) => {
      qc.setQueryData(KEY, res.status);
      await qc.invalidateQueries({ queryKey: KEY });
      setWireError(res.wireError ?? "");
      setPub("");
      setName("");
      setAdding(false);
      setError("");
    },
    onError: (e) => setError(errorText(e, "Could not add the key")),
  });
  const remove = useMutation({
    mutationFn: (fp: string) => del(`/api/system/ssh/keys?fingerprint=${encodeURIComponent(fp)}`),
    onSuccess: () => { setError(""); return qc.invalidateQueries({ queryKey: KEY }); },
    onError: (e) => setError(errorText(e, "Could not remove the key")),
  });

  const st = status.data;
  const keys = st?.keys ?? [];
  const phase = phaseOf(status, st?.keys.length);
  const login = `ssh ${st?.user ?? "root"}@${window.location.hostname}`;

  return (
    <SettingsPage
      status={st && <SshdLine st={st} />}
      description={<>Public keys listed here can log in to this server as <span className="font-mono">root</span>. Add a key to let someone you trust look at the machine directly; remove it when they're done.</>}
      actions={
        !adding && st?.supported && (
          <Button onClick={() => { setAdding(true); setError(""); }}>Add key</Button>
        )
      }
    >
      {phase === "loading" && <Loading label="Checking SSH access…" />}
      {phase === "error" && <LoadError label="Couldn't read SSH access." error={status.error} onRetry={() => status.refetch()} />}

      {wireError && <p className="text-sm text-warn">The key was saved, but sshd isn't using it yet: {wireError}</p>}

      {phase === "empty" && !adding && st?.supported && (
        <p className="text-sm text-ink-muted">No keys — nobody can log in over SSH with a key fog manages.</p>
      )}
      {keys.length > 0 && (
        <ul className="divide-y divide-divider/60 rounded-lg border border-veil-edge bg-veil">
          {keys.map((k) => (
            <li key={k.fingerprint} className="flex items-center gap-3 px-4 py-3">
              <div className="min-w-0 flex-1">
                <p className="text-sm font-medium text-ink">{k.comment || "unnamed key"}</p>
                <p className="break-all font-mono text-xs text-ink-muted">{k.type} · {k.fingerprint}</p>
              </div>
              <Button
                size="sm"
                variant="danger"
                onClick={async () => {
                  if (await ask({ title: `Remove ${k.comment || "this key"}?`, body: "It can no longer log in; open sessions stay connected until they end.", action: "Remove key", danger: true })) remove.mutate(k.fingerprint);
                }}
                disabled={remove.isPending}
              >
                {remove.isPending && remove.variables === k.fingerprint ? "Removing…" : "Remove"}
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
            if (pub.trim()) add.mutate();
          }}
        >
          <TextArea label="Public key" autoFocus rows={3} value={pub} onChange={(e) => setPub(e.target.value)}
            placeholder="ssh-ed25519 AAAAC3Nz… you@laptop" spellCheck={false}
            hint="The contents of the .pub file — never the private key." />
          <div className="max-w-sm">
            <Field label="Name (optional)" value={name} onChange={(e) => setName(e.target.value)} placeholder="the key's own comment" />
          </div>
          <div className="flex items-center gap-3">
            <Button type="submit" variant="primary" disabled={add.isPending || !pub.trim()}>
              {add.isPending ? "Adding…" : "Add key"}
            </Button>
            <Button type="button" onClick={() => { setAdding(false); setPub(""); setName(""); setError(""); }}>Cancel</Button>
          </div>
        </form>
      )}

      {error && <p className="text-sm text-warn">{error}</p>}

      {st?.supported && keys.length > 0 && (
        <p className="flex items-center gap-2 text-xs text-ink-muted">
          Connect with <code className="font-mono text-ink-muted">{login}</code>
          <CopyButton text={login} />
        </p>
      )}
      {dialog}
    </SettingsPage>
  );
}

// What sshd will do with these keys, from its effective configuration.
function SshdLine({ st }: { st: SshAccessStatus }) {
  if (!st.supported) return <StatusLine>SSH access is managed on Linux servers only.</StatusLine>;
  if (!st.sshdInstalled) {
    return <StatusLine tone="warn">sshd isn't installed on this server — install openssh-server to use SSH keys.</StatusLine>;
  }
  if (st.checkError) return <StatusLine tone="warn">{st.checkError}</StatusLine>;
  if (!st.sshdActive) return <StatusLine tone="warn">sshd is installed but not running.</StatusLine>;
  if (st.rootLogin === "no" || st.rootLogin === "forced-commands-only") {
    return <StatusLine tone="warn">sshd is running, but this server's sshd_config has PermitRootLogin {st.rootLogin}, so these keys can't open a shell.</StatusLine>;
  }
  if (!st.wired) {
    return (
      <StatusLine tone={st.keys.length > 0 ? "warn" : "idle"}>
        {st.keys.length > 0 ? "sshd isn't reading fog's keys yet — adding a key sets it up." : "sshd is running. Adding a key sets it up to read fog's keys."}
      </StatusLine>
    );
  }
  return <StatusLine tone="ok">sshd is running and accepts the keys below for root.</StatusLine>;
}
