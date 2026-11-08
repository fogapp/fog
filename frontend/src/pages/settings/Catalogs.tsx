// Settings › App store › Catalogs: the repositories the store is built
// from (catalog_sources). First to last in priority — an app two catalogs
// both have comes from the higher one. Fog's own catalogs are trusted; an
// app from any other is installed held for review. Each row says where the
// store is reading that catalog from and what in it fog couldn't use, so a
// catalog's author can fix it from here.

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, RefreshCw, X } from "lucide-react";
import { post, put, type CatalogResponse, type CatalogStatus } from "../../api";
import Button from "../../components/Button";
import CopyButton from "../../components/CopyButton";
import { errorText } from "../../components/Async";
import Field from "../../components/Field";
import { timeAgo } from "../../lib/format";
import { catalogQuery, settingsQuery } from "../../queries";

type CatalogRemote = Pick<CatalogStatus, "name" | "url" | "branch">;

// fog's own two, as the server's DefaultRemotes has them — offered back
// when the operator has removed one.
const OWN: CatalogRemote[] = [
  { name: "fog-apps", url: "https://github.com/fogapp/catalog.git", branch: "main" },
  { name: "fog-catalog", url: "https://github.com/fogapp/catalog-homelab.git", branch: "main" },
];

const sshURL = (u: string) => u.startsWith("ssh://") || /^[\w.-]+@[\w.-]+:/.test(u);

// nameFor suggests a catalog's name from its URL: the repository's own.
function nameFor(url: string): string {
  const last = url.trim().replace(/\/+$/, "").replace(/\.git$/, "").split(/[/:]/).pop() ?? "";
  return last.toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 40);
}

export default function Catalogs({ data }: { data: CatalogResponse }) {
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const [url, setURL] = useState("");
  const [name, setName] = useState("");
  const [branch, setBranch] = useState("");
  const list: CatalogRemote[] = (data.catalogs ?? []).map(({ name, url, branch }) => ({ name, url, branch }));

  const refresh = () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: catalogQuery.queryKey }),
      qc.invalidateQueries({ queryKey: settingsQuery.queryKey }),
    ]);

  // A saved list is fetched straight away, so a catalog just added shows
  // its apps (or why it has none) when the save settles.
  const save = useMutation({
    mutationFn: async (next: CatalogRemote[]) => {
      await put("/api/settings", { catalog_sources: JSON.stringify(next) });
      await post("/api/catalog/sync", {});
    },
    onSuccess: async () => {
      await refresh();
      setError("");
    },
    onError: async (e) => {
      setError(errorText(e, "Saving the catalogs failed"));
      await refresh();
    },
  });
  const sync = useMutation({
    mutationFn: () => post("/api/catalog/sync", {}),
    onSuccess: refresh,
    onError: (e) => setError(errorText(e, "Fetching failed")),
  });
  const busy = save.isPending || sync.isPending;

  function move(i: number, by: number) {
    const next = [...list];
    const [row] = next.splice(i, 1);
    next.splice(i + by, 0, row);
    save.mutate(next);
  }

  function add() {
    const n = name.trim() || nameFor(url);
    save.mutate([...list, { name: n, url: url.trim(), branch: branch.trim() || "main" }], {
      onSuccess: () => {
        setURL("");
        setName("");
        setBranch("");
      },
    });
  }

  const missingOwn = OWN.filter((o) => !list.some((r) => r.name === o.name));
  // The key is shown whenever a catalog needs it or might: one cloned over
  // SSH, or one whose fetch failed (the way out for a private catalog given
  // as https is its SSH URL and this key).
  const anySSH = list.some((r) => sshURL(r.url)) || sshURL(url) || (data.catalogs ?? []).some((c) => c.error);

  return (
    <section className="space-y-3 rounded-lg border border-veil-edge bg-veil p-4">
      <div className="flex flex-wrap items-start gap-3">
        <div className="min-w-0 flex-1">
          <h3 className="font-medium text-ink">Catalogs</h3>
          <p className="mt-0.5 text-sm text-ink-muted">
            The store is built from these repositories, first to last: an app two of them both have comes from the higher one.
            Apps from a catalog that isn't one of fog's own are held for review when installed.
          </p>
        </div>
        <Button size="sm" onClick={() => sync.mutate()} disabled={busy}>
          <RefreshCw className={sync.isPending ? "animate-spin" : ""} />
          {sync.isPending ? "Fetching…" : "Fetch now"}
        </Button>
      </div>

      <ul className="divide-y divide-divider rounded-md border border-hair">
        {(data.catalogs ?? []).map((c, i) => (
          <CatalogRow key={c.name} c={c} first={i === 0} last={i === list.length - 1} busy={busy}
            onUp={() => move(i, -1)} onDown={() => move(i, 1)}
            onRemove={() => save.mutate(list.filter((r) => r.name !== c.name))} />
        ))}
        {list.length === 0 && (
          <li className="px-3 py-4 text-center text-sm text-ink-muted">No catalogs — the store is empty.</li>
        )}
      </ul>
      {missingOwn.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {missingOwn.map((o) => (
            <Button key={o.name} size="sm" variant="ghost" disabled={busy} onClick={() => save.mutate([...list, o])}>
              + Add fog's {o.name}
            </Button>
          ))}
        </div>
      )}

      <form className="grid gap-3 sm:grid-cols-[minmax(0,1fr)_10rem_7rem_auto] sm:items-end"
        onSubmit={(e) => {
          e.preventDefault();
          if (url.trim()) add();
        }}>
        <Field label="Add a catalog" value={url} onChange={(e) => setURL(e.target.value)}
          placeholder="https://github.com/someone/fog-apps.git" />
        <Field label="Name" value={name} onChange={(e) => setName(e.target.value)} placeholder={nameFor(url) || "name"} />
        <Field label="Branch" value={branch} onChange={(e) => setBranch(e.target.value)} placeholder="main" />
        <Button type="submit" variant="primary" disabled={busy || !url.trim()}>
          {save.isPending ? "Adding…" : "Add"}
        </Button>
      </form>
      {anySSH && data.catalogKey && (
        <div className="space-y-1">
          <p className="text-xs text-ink-muted">
            A catalog cloned over SSH is fetched with this key — add it to the repository as a read-only deploy key.
          </p>
          <div className="flex items-center gap-2">
            <code className="min-w-0 flex-1 truncate rounded bg-fog-900 px-2 py-1 font-mono text-xs text-ink-muted">{data.catalogKey}</code>
            <CopyButton text={data.catalogKey} />
          </div>
        </div>
      )}
      {error && <p className="text-sm text-warn">{error}</p>}
    </section>
  );
}

function CatalogRow({ c, first, last, busy, onUp, onDown, onRemove }: {
  c: CatalogStatus;
  first: boolean;
  last: boolean;
  busy: boolean;
  onUp: () => void;
  onDown: () => void;
  onRemove: () => void;
}) {
  const where =
    c.from === "clone" ? `${c.commit ? `at ${c.commit.slice(0, 8)}` : "fetched"}${c.syncedAt ? ` · checked ${timeAgo(c.syncedAt)}` : ""}`
    : c.from === "seed" ? "the copy this fog shipped with — not fetched yet"
    : "nothing to read yet";
  const problems = c.problems ?? [];
  return (
    <li className="space-y-1.5 px-3 py-2.5">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-sm text-ink">
            <span className="font-medium">{c.title || c.name}</span>
            {c.title && c.title !== c.name && <span className="ml-2 font-mono text-xs text-ink-muted">{c.name}</span>}
            {c.trusted && (
              <span className="ml-2 rounded-full bg-accent/10 px-2 py-0.5 text-[11px] font-medium text-accent">fog's own</span>
            )}
          </p>
          <p className="truncate font-mono text-xs text-ink-muted" title={c.url}>
            {c.url}{c.branch && c.branch !== "main" ? ` · ${c.branch}` : ""}
          </p>
          <p className="text-xs text-ink-muted">
            {c.apps} app{c.apps === 1 ? "" : "s"} · {where}
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <Button size="sm" variant="ghost" aria-label={`Move ${c.name} up`} disabled={busy || first} onClick={onUp}><ArrowUp /></Button>
          <Button size="sm" variant="ghost" aria-label={`Move ${c.name} down`} disabled={busy || last} onClick={onDown}><ArrowDown /></Button>
          <Button size="sm" variant="ghost" aria-label={`Remove ${c.name}`} disabled={busy} onClick={onRemove}><X /></Button>
        </div>
      </div>
      {c.error && <p className="text-xs text-warn">{c.error}</p>}
      {c.detail && (
        <details className="text-xs text-ink-muted">
          <summary className="cursor-pointer">What git said</summary>
          <p className="mt-1 break-all pl-4 font-mono">{c.detail}</p>
        </details>
      )}
      {problems.length > 0 && (
        <details className="text-xs text-ink-muted">
          <summary className="cursor-pointer">
            {problems.length} thing{problems.length === 1 ? "" : "s"} in this catalog fog couldn't use
          </summary>
          <ul className="mt-1 space-y-0.5 pl-4">
            {problems.map((p, i) => (
              <li key={i}><span className="font-mono">{p.app || "catalog.yaml"}</span>: {p.error}</li>
            ))}
          </ul>
        </details>
      )}
    </li>
  );
}
