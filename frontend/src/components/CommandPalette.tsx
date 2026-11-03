// ⌘K: jump to anything. Apps and their tabs, every Settings page (by name
// and by the words people search for), the Inbox tabs, the doors of Add app — and, when an app is
// open, the verbs that act on it. One list, typed against, arrows and Enter
// to go. The keyboard model is the old sidebar combobox's; the reach is the
// whole product. Also the home of the g-shortcuts (g h, g i, g f, g s).

import { useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, type FileNode } from "../api";
import FileIcon from "./FileIcon";
import { categoryOf } from "../lib/fileKinds";
import { errorText } from "./Async";
import { StateDot } from "./StateBadge";
import { useDismissable } from "../lib/useDismissable";
import {
  APP_SECTIONS, APP_SECTION_LABELS, INBOX_TABS, INBOX_TAB_LABELS,
  appNameFromPath, paths,
} from "../lib/paths";
import { SETTINGS_AREAS, type SettingsTo } from "../lib/settings";
import { appsKey, useApps } from "../lib/useApps";
import { displayName } from "../lib/displayName";

interface Item {
  id: string;
  label: string;
  hint?: string;
  group: "Apps" | "This app" | "Go to" | "Settings" | "Actions" | "Drive";
  // Other words that should find it: "postgres" finds Storage › Database.
  terms?: string;
  // Every item either navigates or runs; never both.
  to?: string;
  run?: () => void;
  dot?: React.ReactNode;
}

// Subsequence match: "hlg" finds "hub › Logs". Scored so a prefix beats a
// scattered match and a shorter label beats a longer one, which keeps the
// app itself above its own tabs when you type just its name.
function score(label: string, q: string): number {
  const l = label.toLowerCase();
  if (!q) return 1;
  if (l.startsWith(q)) return 100 - l.length / 100;
  if (l.includes(q)) return 60 - l.length / 100;
  let i = 0;
  for (const ch of l) if (ch === q[i]) i++;
  return i === q.length ? 20 - l.length / 100 : 0;
}

function termScore(terms: string | undefined, q: string): number {
  if (!terms || !q) return 0;
  return terms.split(" ").some((t) => t.startsWith(q)) ? 30 : 0;
}

// The header's search box opens the palette the way ⌘K does. The palette
// lives once, in the shell; anything else asks for it by this event rather
// than reaching into its state.
const OPEN_EVENT = "fog:command-palette";
export function openCommandPalette() {
  window.dispatchEvent(new Event(OPEN_EVENT));
}

export default function CommandPalette({ basic }: { basic?: boolean }) {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const [hi, setHi] = useState(0);
  const [note, setNote] = useState("");
  const wrap = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const navigate = useNavigate();
  const location = useLocation();
  const qc = useQueryClient();
  const apps = useApps({ refetchInterval: 15000, enabled: open });
  // Two characters in, the query also runs against file names — the one
  // list reaches into the tree.
  const fileQ = q.trim();
  const fileHits = useQuery({
    queryKey: ["files", "search", "", fileQ, "palette"],
    queryFn: () => get<{ entries: FileNode[] }>(`/api/files/search?q=${encodeURIComponent(fileQ)}&limit=8`),
    enabled: open && !basic && fileQ.length >= 2,
    placeholderData: (prev) => prev,
    staleTime: 10_000,
  });
  const current = appNameFromPath(location.pathname);
  const currentStatus = apps.data?.find((a) => a.name === current);

  const act = useMutation({
    mutationFn: ({ name, action }: { name: string; action: string }) =>
      post(`/api/apps/${name}/actions`, { action }),
    onSuccess: () => {
      setNote("");
      return qc.invalidateQueries({ queryKey: appsKey });
    },
    onError: (e) => setNote(errorText(e, "Action failed")),
  });
  const backup = useMutation({
    mutationFn: (name: string) => post(`/api/apps/${name}/backups`),
    onSuccess: (_d, name) => setNote(`Backed up ${name} ✓`),
    onError: (e) => setNote(errorText(e, "Backup failed")),
  });

  // ⌘K / Ctrl-K opens; Escape and click-away close (useDismissable). The
  // g-shortcuts fire only when nothing editable has focus.
  useEffect(() => {
    let pendingG = 0;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((o) => !o);
        return;
      }
      if (basic || e.metaKey || e.ctrlKey || e.altKey) return;
      const t = e.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable)) return;
      const now = Date.now();
      if (e.key === "g") {
        pendingG = now;
        return;
      }
      if (now - pendingG < 1500) {
        const go: Record<string, string> = { h: paths.home, i: paths.inbox(), f: paths.federation, d: paths.files(), s: paths.settings(), a: paths.newApp };
        if (go[e.key]) {
          e.preventDefault();
          navigate(go[e.key]);
        }
        pendingG = 0;
      }
    };
    const onOpen = () => setOpen(true);
    document.addEventListener("keydown", onKey);
    window.addEventListener(OPEN_EVENT, onOpen);
    return () => {
      document.removeEventListener("keydown", onKey);
      window.removeEventListener(OPEN_EVENT, onOpen);
    };
  }, [basic, navigate]);

  useEffect(() => {
    if (open) {
      setQ("");
      setHi(0);
      setNote("");
      // Focus after the panel paints.
      setTimeout(() => input.current?.focus(), 0);
    }
  }, [open]);

  useDismissable(wrap, open, () => setOpen(false));

  const items = useMemo((): Item[] => {
    const out: Item[] = [];
    for (const a of apps.data ?? []) {
      out.push({ id: `app:${a.name}`, label: a.name, group: "Apps", to: paths.app(a.name), dot: <StateDot app={a} /> });
    }
    if (current) {
      for (const s of APP_SECTIONS) {
        out.push({ id: `sec:${s}`, label: `${current} › ${APP_SECTION_LABELS[s]}`, group: "This app", to: paths.app(current, s) });
      }
      const running = currentStatus?.state === "running";
      const verbs: [string, string, boolean][] = [
        ["restart", "Restart", running],
        ["stop", "Stop", running],
        ["start", "Start", !!currentStatus && !running],
        ["deploy", "Redeploy", !!currentStatus],
      ];
      for (const [action, label, ok] of verbs) {
        if (ok) out.push({ id: `act:${action}`, label: `${label} ${current}`, group: "Actions", run: () => act.mutate({ name: current, action }) });
      }
      if (currentStatus) out.push({ id: "act:backup", label: `Back up ${current} now`, group: "Actions", run: () => backup.mutate(current) });
      const host = currentStatus?.hosts?.[0];
      if (running && host) out.push({ id: "act:open", label: `Open ${current} ↗`, group: "Actions", run: () => window.open(`https://${host}`, "_blank", "noreferrer") });
    }
    if (!basic) {
      out.push({ id: "go:home", label: "Home", group: "Go to", to: paths.home });
      for (const t of INBOX_TABS) out.push({ id: `go:inbox:${t}`, label: `Inbox › ${INBOX_TAB_LABELS[t]}`, group: "Go to", to: paths.inbox(t) });
      out.push({ id: "go:chat", label: "Chat", group: "Go to", to: paths.chat() });
      out.push({ id: "go:files", label: "Drive", group: "Go to", to: paths.files() });
      out.push({ id: "go:shared", label: "Drive › Shared", group: "Go to", to: paths.filesShared() });
      out.push({ id: "go:trash", label: "Drive › Trash", group: "Go to", to: paths.trash });
      out.push({ id: "go:fed", label: "Connect", group: "Go to", to: paths.federation });
    }
    out.push({ id: "go:new", label: "Add app › App store", group: "Go to", to: paths.newApp });
    out.push({ id: "go:new:git", label: "Add app › Your repository", group: "Go to", to: `${paths.newApp}?from=git` });
    out.push({ id: "go:new:image", label: "Add app › Image or compose", group: "Go to", to: `${paths.newApp}?from=image` });
    if (!basic) {
      out.push({ id: "set:", label: "Settings", group: "Settings", to: paths.settings() });
      for (const a of SETTINGS_AREAS) {
        for (const p of a.pages) {
          out.push({
            id: `set:${a.id}/${p.id}`, label: `Settings › ${a.label} › ${p.label}`, group: "Settings",
            to: paths.settings(`${a.id}/${p.id}` as SettingsTo), terms: p.terms,
          });
        }
      }
    }
    return out;
  }, [apps.data, current, currentStatus, basic, act, backup]);

  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    // A page found by one of its words ranks under one found by name, and
    // only a whole-word start counts — a subsequence of a word list would
    // match nearly anything.
    const scored = items
      .map((it) => ({ it, s: Math.max(score(it.label, needle), termScore(it.terms, needle)) }))
      .filter((x) => x.s > 0);
    const base = needle ? scored.sort((a, b) => b.s - a.s).map((x) => x.it) : scored.map((x) => x.it);
    const files: Item[] = needle.length >= 2 && fileHits.data
      ? fileHits.data.entries.map((n) => {
          const dir = n.path.includes("/") ? n.path.slice(0, n.path.lastIndexOf("/")) : "";
          return {
            id: `file:${n.path}`,
            label: displayName(n.name),
            hint: dir ? displayName(dir) : "/",
            group: "Drive" as const,
            to: n.kind === "dir" ? paths.files(n.path) : `${paths.files(dir)}?select=${encodeURIComponent(n.name)}`,
            dot: <FileIcon category={categoryOf(n)} className="h-4 w-4" />,
          };
        })
      : [];
    return [...base.slice(0, 32), ...files].slice(0, 40);
  }, [items, q, fileHits.data]);

  useEffect(() => setHi(0), [q]);

  function choose(it: Item) {
    setOpen(false);
    if (it.to) navigate(it.to);
    else it.run?.();
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setHi((h) => Math.min(h + 1, shown.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setHi((h) => Math.max(h - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (shown[hi]) choose(shown[hi]);
    }
  }

  if (!open) return null;

  let lastGroup = "";
  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/50 px-4 pt-[12vh]">
      <div ref={wrap} className="w-full max-w-xl overflow-hidden rounded-xl border border-veil-edge bg-fog-900 shadow-2xl shadow-black/50">
        <input
          ref={input}
          role="combobox"
          aria-expanded="true"
          aria-autocomplete="list"
          aria-controls="command-palette-list"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={onKey}
          placeholder="Jump to an app, a page, a setting — or act on this app…"
          className="w-full border-b border-hair bg-transparent px-4 py-3 text-sm text-ink placeholder-ink-muted outline-none"
        />
        <ul id="command-palette-list" role="listbox" className="max-h-[50vh] overflow-y-auto py-1">
          {apps.isLoading && (apps.data ?? []).length === 0 && (
            <li className="px-4 py-2 text-sm text-ink-muted">Loading apps…</li>
          )}
          {shown.length === 0 && !apps.isLoading && (
            <li className="px-4 py-2 text-sm text-ink-muted">
              {fileHits.isFetching ? "Searching files…" : <>Nothing matches “{q.trim()}”.</>}
            </li>
          )}
          {shown.map((it, i) => {
            const header = it.group !== lastGroup ? it.group : null;
            lastGroup = it.group;
            return (
              <li key={it.id} role="option" aria-selected={i === hi}>
                {header && (
                  <p className="group-label px-4 pb-0.5 pt-2">{header}</p>
                )}
                <button
                  onClick={() => choose(it)}
                  onPointerMove={() => setHi(i)}
                  className={`flex w-full items-center gap-2 px-4 py-1.5 text-left text-sm ${
                    i === hi ? "bg-fog-800 text-ink" : "text-ink"
                  }`}
                >
                  {it.dot}
                  <span className="truncate">{it.label}</span>
                  {it.hint && <span className="ml-auto text-xs text-ink-muted">{it.hint}</span>}
                </button>
              </li>
            );
          })}
        </ul>
        <div className="flex items-center justify-between border-t border-hair px-4 py-1.5 text-[11px] text-ink-muted">
          <span>{note || "↑↓ to move · ↵ to go · esc to close"}</span>
          <span>g h · g i · g d · g f · g s · g a</span>
        </div>
      </div>
    </div>
  );
}
