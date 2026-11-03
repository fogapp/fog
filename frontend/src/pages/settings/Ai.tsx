// Everything about the MCP server in one place: what it is, the tokens that
// gate it, how to connect a client, what the tools can and cannot do, and
// what to check when a connection stops working. Token management lives here
// because the token is the whole setup — a page that
// explains MCP but makes you hunt for the key would leave most operators
// stranded at step one.
//
// Tokens are per client, not per box: one for Claude Code on a laptop,
// another for an OpenClaw instance. Each can be revoked on its own, and the
// audit log names the one that acted.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, post, type MCPToken } from "../../api";
import { LoadError, Loading, errorText } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import CopyButton from "../../components/CopyButton";
import Field from "../../components/Field";
import { Section, StatusLine } from "../../components/Section";
import SettingRow, { RowItem, RowList, SettingRows } from "../../components/SettingRow";
import SettingsLink from "../../components/SettingsLink";
import Toggle from "../../components/Toggle";
import { SettingsPage } from "./Frame";
import { mcpTokensQuery } from "../../queries";

const READ_TOOLS: [string, string][] = [
  ["fog_apps_list", "every app with live state, health, resource use, and pending updates"],
  ["fog_app_get", "one app in depth: source, deploy history, and a diagnosis when the last deploy failed"],
  ["fog_app_logs", "the tail of an app's container logs"],
  ["fog_issues_list", "the configuration scanner's findings, with suggested fixes"],
  ["fog_issues_summary", "open issue counts by severity"],
  ["fog_system_status", "Fog version, app counts, CPU / memory / disk / load"],
  ["fog_backups_list", "an app's recorded backups"],
  ["fog_audit_recent", "the audit log: who did what, when"],
  ["fog_files_list / fog_files_search", "browse and search the Drive tree by name — except Apps/ and hidden files, unless the token may read them"],
  ["fog_files_read", "read a text file from Drive (Markdown, JSON, YAML, code — up to 256 KiB), with the same exceptions"],
  ["fog_chat_rooms / fog_chat_read / fog_chat_search", "the rooms fog itself is in, and their messages — nothing else in chat is readable here; every other room is sealed to its members' devices"],
];

const MANAGE_TOOLS: [string, string][] = [
  ["fog_app_control", "start, stop, or restart an app"],
  ["fog_app_deploy", "deploy the latest revision, or roll back to a past one"],
  ["fog_issue_ack / fog_issue_unack", "acknowledge an issue, or put it back to open"],
  ["fog_issues_scan", "run a configuration scan now"],
  ["fog_app_backup_set", "change an app's backup settings — on or off, mode, hour, before-update snapshot"],
  ["fog_updates_check", "check registries and git remotes for app updates (reports only — applies nothing)"],
  ["fog_chat_send", "post into a room fog is in, as fog — only with a token created with \"may post to chat\""],
];

export default function Ai() {
  const qc = useQueryClient();
  const list = useQuery(mcpTokensQuery);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  // The newly minted plaintext, kept next to the token it belongs to so a
  // second create can't leave the first one's secret labelled wrongly.
  const [fresh, setFresh] = useState<{ id: number; name: string; token: string } | null>(null);
  const [ask, dialog] = useConfirm();

  const revoke = useMutation({
    mutationFn: (id: number) => del(`/api/settings/mcp-tokens/${id}`),
    onSuccess: (_data, id) => {
      setError("");
      // Drop a plaintext still on screen for the token just revoked — it
      // stopped working, and showing it would claim otherwise.
      if (fresh?.id === id) setFresh(null);
      return qc.invalidateQueries({ queryKey: mcpTokensQuery.queryKey });
    },
    onError: (e) => setError(errorText(e, "Couldn't revoke the token")),
  });

  const tokens = list.data?.tokens;

  return (
    <SettingsPage
      description={
        <>
          Fog speaks the Model Context Protocol (MCP) — the standard AI assistants use to work with tools. Connect a
          client like Claude Code and ask questions ("what's wrong with my server?") or hand off chores ("restart
          Jellyfin") in plain language. Every action it takes is attributed in the audit log.
        </>
      }
      status={
        !tokens ? undefined : tokens.length === 0 ? (
          <StatusLine>Off — the MCP endpoint refuses every connection until a token exists.</StatusLine>
        ) : (
          <StatusLine tone="ok">On — {tokens.length} client{tokens.length === 1 ? "" : "s"} can connect.</StatusLine>
        )
      }
      actions={!creating && <Button variant="primary" onClick={() => { setCreating(true); setFresh(null); }}>New token</Button>}
    >
      <Section
        id="tokens"
        title="Tokens"
        description="One per client — name it after the thing that will hold it, so the audit log and this list stay readable."
      >
        {creating && (
          <TokenForm
            onCreated={(t) => {
              setFresh(t);
              setCreating(false);
            }}
            onCancel={() => setCreating(false)}
          />
        )}
        {fresh && (
          <div className="space-y-2 rounded-lg border border-accent/30 bg-accent/10 p-4 text-sm">
            <p className="text-accent">
              The token for <span className="font-mono">{fresh.name}</span> is shown once — copy it now. Only a digest is
              stored; if it's lost, revoke it and create another.
            </p>
            <CopyRow text={fresh.token} />
            <Button variant="ghost" size="sm" onClick={() => setFresh(null)}>Dismiss</Button>
          </div>
        )}
        {!tokens && !list.isError && <Loading label="Loading tokens…" />}
        {list.isError && <LoadError label="Couldn't load the token list." error={list.error} onRetry={() => list.refetch()} />}
        {tokens && tokens.length === 0 && !creating && <p className="text-sm text-ink-muted">No tokens — AI access is off.</p>}
        {tokens && tokens.length > 0 && (
          <RowList>
            {tokens.map((t) => (
              <RowItem
                key={t.id}
                title={
                  <span className="flex items-center gap-2">
                    <span className="truncate">{t.name}</span>
                    {t.canPostChat && (
                      <span className="rounded-full border border-accent/30 px-1.5 text-[11px] font-normal text-accent" title="This token may post into rooms fog is in">
                        posts to chat
                      </span>
                    )}
                    {t.canReadAllFiles && (
                      <span className="rounded-full border border-warn/30 px-1.5 text-[11px] font-normal text-warn" title="This token's Drive tools reach Apps/ and hidden files">
                        reads app data
                      </span>
                    )}
                  </span>
                }
                meta={
                  <>
                    Created {new Date(t.createdAt).toLocaleDateString()} ·{" "}
                    {t.lastUsedAt ? `last used ${new Date(t.lastUsedAt).toLocaleString()}` : "never used"}
                  </>
                }
                actions={
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={revoke.isPending && revoke.variables === t.id}
                    onClick={async () => {
                      if (await ask({ title: `Revoke the token for ${t.name}?`, body: "That client is cut off immediately.", action: "Revoke", danger: true })) {
                        revoke.mutate(t.id);
                      }
                    }}
                  >
                    {revoke.isPending && revoke.variables === t.id ? "Revoking…" : "Revoke"}
                  </Button>
                }
              />
            ))}
          </RowList>
        )}
        {error && <p className="text-sm text-warn">{error}</p>}
      </Section>

      <ConnectSection />

      <Section id="tools" title="What it can do">
        <ToolList title="Read" tools={READ_TOOLS} />
        <ToolList title="Manage" tools={MANAGE_TOOLS} />
        <p className="text-xs text-ink-muted">
          Deliberately <span className="text-ink-muted">not</span> exposed: deleting apps, editing app configuration
          (beyond backup settings), restoring backups, rebooting, self-update, and shell access. Those stay in this UI, where a misread instruction
          can't reach them.
        </p>
      </Section>

      <Section id="security" title="Security">
        <ul className="list-disc space-y-1.5 pl-5 text-xs text-ink-muted">
          <li>
            One token per client. Fog stores only a digest — like a password, the plaintext is shown once at creation and
            can't be recovered, only replaced by a new token.
          </li>
          <li>
            Every change made through MCP lands in the audit log (<SettingsLink to="server/activity" />) as actor{" "}
            <span className="font-mono text-ink-muted">mcp:&lt;token name&gt;</span>, so you can always see which
            assistant did what.
          </li>
          <li>Requests are rate limited; revoking a token cuts off that client immediately.</li>
          <li>
            A token is a credential — treat it like a password. Anyone holding one can start, stop, and deploy apps on
            this box, and read Drive.
          </li>
          <li>
            Drive reads leave out <span className="font-mono text-ink-muted">Apps/</span> and hidden files unless the
            token was created with "may read app data and hidden files" — including through links that point into them.
          </li>
        </ul>
      </Section>

      <Section id="troubleshooting" title="If it stops working">
        <ul className="list-disc space-y-1.5 pl-5 text-xs text-ink-muted">
          <li>
            <span className="text-ink-muted">401 / unauthorized</span> — that client's token was revoked. Create a new one
            above and update the client.
          </li>
          <li>
            <span className="text-ink-muted">Can't reach the server</span> — the URL must be reachable from wherever the
            client runs. On your LAN that's this page's address; from outside, a Cloudflare Tunnel hostname (
            <SettingsLink to="network/tunnel" />) works too.
          </li>
          <li>
            <span className="text-ink-muted">429 / rate limited</span> — an assistant is calling in a tight loop; it clears
            within a minute. The limit is shared by all clients.
          </li>
        </ul>
      </Section>
      {dialog}
    </SettingsPage>
  );
}

// The token lifecycle. A plaintext exists exactly once, in the create
// response — it's shown until the operator dismisses it or navigates away,
// and only its digest is stored, so there is no "show token again": a lost
// token is revoked and replaced.
function TokenForm({ onCreated, onCancel }: {
  onCreated: (t: { id: number; name: string; token: string }) => void;
  onCancel: () => void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [canPostChat, setCanPostChat] = useState(false);
  const [canReadAllFiles, setCanReadAllFiles] = useState(false);
  const [error, setError] = useState("");
  const create = useMutation({
    mutationFn: (clientName: string) =>
      post<MCPToken & { token: string }>("/api/settings/mcp-tokens", { name: clientName, canPostChat, canReadAllFiles }),
    onSuccess: (created) => {
      setError("");
      onCreated({ id: created.id, name: created.name, token: created.token });
      return qc.invalidateQueries({ queryKey: mcpTokensQuery.queryKey });
    },
    onError: (e) => setError(errorText(e, "Couldn't create the token")),
  });
  return (
    <form
      className="space-y-4 rounded-lg border border-veil-edge bg-veil p-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (name.trim()) create.mutate(name.trim());
      }}
    >
      <div className="max-w-sm">
        <Field label="Client name" value={name} onChange={(e) => setName(e.target.value)} maxLength={40} placeholder="claude-code" autoFocus />
      </div>
      <SettingRows>
        <SettingRow
          label="May post to chat"
          hint="The assistant can send messages, as fog, into rooms people have added fog to. Reading those rooms needs no extra right; every other room is unreadable here."
        >
          <Toggle checked={canPostChat} onChange={setCanPostChat} aria-label="May post to chat" />
        </SettingRow>
        <SettingRow
          label="May read app data and hidden files"
          hint="Without this, the Drive tools skip Apps/ — every app's own data, including the passwords and keys apps keep there — and hidden (dot) files. Turn it on only for an assistant you'd hand those to."
        >
          <Toggle checked={canReadAllFiles} onChange={setCanReadAllFiles} aria-label="May read app data and hidden files" />
        </SettingRow>
      </SettingRows>
      <div className="flex items-center gap-2">
        <Button type="submit" variant="primary" disabled={create.isPending || !name.trim()}>
          {create.isPending ? "Creating…" : "Create token"}
        </Button>
        <Button type="button" onClick={onCancel}>Cancel</Button>
      </div>
      {error && <p className="text-sm text-warn">{error}</p>}
    </form>
  );
}

function ConnectSection() {
  const list = useQuery(mcpTokensQuery);
  const endpoint = `${window.location.origin}/mcp`;
  const snippet = `claude mcp add --transport http fog ${endpoint} --header "Authorization: Bearer <token>"`;

  return (
    <Section id="connect" title="Connect a client">
      {list.data && list.data.tokens.length === 0 && (
        <p className="text-xs text-warn">Create a token first — without one, every connection is refused.</p>
      )}
      <p className="text-xs text-ink-muted">
        <span className="text-ink">Claude Code</span> (terminal): run this once, with{" "}
        <span className="font-mono text-ink">&lt;token&gt;</span> replaced by that client's token. Then just talk to
        it — "list my apps", "any critical issues?".
      </p>
      <CopyRow text={snippet} />
      <p className="text-xs text-ink-muted">
        <span className="text-ink">Any other MCP client</span>: the endpoint is{" "}
        <span className="font-mono text-ink">{endpoint}</span> (streamable HTTP transport) with header{" "}
        <span className="font-mono text-ink">Authorization: Bearer &lt;token&gt;</span>. For Claude Desktop or
        claude.ai, add it as a custom connector with that URL — note those run outside your network, so the endpoint must
        be reachable from the internet (for example via a Cloudflare Tunnel hostname).
      </p>
    </Section>
  );
}

// CopyButton carries the insecure-context fallback this page needs most —
// on http://fog.lan navigator.clipboard doesn't exist, and this row shows
// the one-time MCP token. It also refuses to say "Copied ✓" for a copy
// that didn't happen.
function CopyRow({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-2">
      <code className="min-w-0 flex-1 break-all rounded-md bg-fog-950 p-2 font-mono text-xs text-ink">{text}</code>
      <CopyButton text={text} />
    </div>
  );
}

function ToolList({ title, tools }: { title: string; tools: [string, string][] }) {
  return (
    <div className="space-y-1.5">
      <h3 className="text-sm font-medium text-ink">{title}</h3>
      <ul className="space-y-1 text-xs text-ink-muted">
        {tools.map(([name, desc]) => (
          <li key={name} className="flex gap-2">
            <span className="shrink-0 font-mono text-ink-muted">{name}</span>
            <span>— {desc}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}
