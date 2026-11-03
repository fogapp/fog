// Picking a folder in Drive without typing a path. Three places are worth
// offering by name: Content, where the household's own libraries live and
// where a media folder almost always belongs; the app's own folder under
// Apps, which is where its data belongs; and — when the volume already
// points somewhere that is neither — the folder it sits in, so a change
// stays near where it was. Each group offers the folders actually inside it
// and a way to make another, so "the Films folder inside Content" is a
// click rather than a hand-typed path.
//
// New folder makes the folder for real, parents included: it is in Drive to
// drop files into before the app ever deploys, and it shows up in the list
// beside the others rather than only in this box.

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, get, post, type FilesListing } from "../api";
import { filesKey } from "../lib/useFiles";
import { APPS_DIR, CONTENT_DIR, appFolderFor, driveLibraryOf } from "../lib/volumePlacement";
import { useAction } from "../lib/useAction";
import { control } from "./Field";
import { buttonClass } from "./Button";

const dirsKey = (path: string) => ["files", "app-folders", path] as const;

function useDirs(path: string, enabled: boolean) {
  return useQuery({
    queryKey: dirsKey(path),
    queryFn: () => get<FilesListing>(`/api/files/ls?path=${encodeURIComponent(path)}&limit=200`),
    enabled: enabled && !!path,
    retry: false,
  });
}

export default function AppFolderChoices({ app, rel, onPick }: {
  // The app being installed or edited; "" where the name isn't known yet,
  // which leaves Content and the volume's own folder to offer.
  app: string;
  rel: string;
  onPick: (rel: string) => void;
}) {
  const qc = useQueryClient();
  const [adding, setAdding] = useState<string | null>(null);
  const [name, setName] = useState("");
  const { run, busy, error, setError } = useAction();

  // The app's own folder by however it is really spelled in Apps: "paperless"
  // finds an existing Apps/Paperless rather than proposing a second folder
  // beside it.
  const appsQ = useDirs(APPS_DIR, !!app.trim());
  const spelled = (appsQ.data?.entries ?? []).find(
    (e) => e.kind === "dir" && e.name.toLowerCase() === app.trim().toLowerCase(),
  );
  const appBase = app.trim() ? spelled?.path ?? appFolderFor(app) : "";
  // The folder the volume already sits in, when it is neither of the two
  // reserved ones' own children — a library it shares, or a folder of the
  // operator's own.
  const libBase = driveLibraryOf(rel);

  const contentQ = useDirs(CONTENT_DIR, true);
  const appQ = useDirs(appBase, !!appBase);
  const libQ = useDirs(libBase, !!libBase && libBase !== appBase && libBase !== CONTENT_DIR);

  const groups: { base: string; why: string; q: ReturnType<typeof useDirs> }[] = [
    { base: CONTENT_DIR, why: "shared libraries", q: contentQ },
  ];
  if (appBase) groups.push({ base: appBase, why: "this app's own data", q: appQ });
  if (libBase && libBase !== appBase && libBase !== CONTENT_DIR) {
    groups.push({ base: libBase, why: "where this folder is now", q: libQ });
  }

  const create = async (base: string) => {
    const leaf = name.trim().replace(/^\/+|\/+$/g, "");
    if (!leaf) return;
    await run(async () => {
      // Mkdir makes one folder; the app's own may not be there yet, so every
      // step down from the base is made in turn and one that is already
      // there (409) is simply the step we wanted.
      const segs = `${base}/${leaf}`.split("/").filter(Boolean);
      for (let i = 1; i <= segs.length; i++) {
        try {
          await post("/api/files/mkdir", { path: segs.slice(0, i).join("/") });
        } catch (e) {
          if (!(e instanceof ApiError && e.status === 409)) throw e;
        }
      }
      await qc.invalidateQueries({ queryKey: filesKey });
      onPick(segs.join("/"));
      setAdding(null);
      setName("");
    }, "Could not create the folder");
  };

  return (
    <div className="space-y-2">
      {groups.map((g) => {
        const dirs = (g.q.data?.entries ?? []).filter((e) => e.kind === "dir");
        const missing = g.q.isError && g.q.error instanceof ApiError && g.q.error.status === 404;
        return (
          <div key={g.base} className="space-y-1">
            <p className="text-[11px] text-ink-muted">
              <span className="font-mono text-ink-muted">Drive/{g.base}</span> — {g.why}
              {missing && " · not there yet; picking it here creates it on deploy"}
            </p>
            <div className="flex flex-wrap items-center gap-1.5">
              <Chip on={rel === g.base} onClick={() => onPick(g.base)}>the folder itself</Chip>
              {g.q.isPending ? (
                <span className="text-[11px] text-ink-muted">reading the folder…</span>
              ) : (
                dirs.map((d) => (
                  <Chip key={d.path} on={rel === d.path} onClick={() => onPick(d.path)}>
                    {d.name}
                  </Chip>
                ))
              )}
              {adding === g.base ? (
                <span className="flex items-center gap-1">
                  <span className="font-mono text-[11px] text-ink-muted">{g.base}/</span>
                  <input
                    autoFocus
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") { e.preventDefault(); create(g.base); }
                      if (e.key === "Escape") { setAdding(null); setName(""); }
                    }}
                    placeholder="folder name"
                    className={`${control} w-36`}
                  />
                  <button type="button" onClick={() => create(g.base)} disabled={busy || !name.trim()}
                    className={buttonClass()}>
                    {busy ? "Creating…" : "Create"}
                  </button>
                  <button type="button" onClick={() => { setAdding(null); setName(""); }}
                    className="text-xs text-ink-muted hover:text-ink">
                    Cancel
                  </button>
                </span>
              ) : (
                <button type="button" onClick={() => { setAdding(g.base); setName(""); setError(""); }}
                  className="rounded-md border border-dashed border-edge px-2.5 py-1 text-xs font-medium text-ink-muted hover:border-fog-500 hover:text-ink">
                  + New folder…
                </button>
              )}
            </div>
          </div>
        );
      })}
      {error && <p className="text-xs text-warn">{error}</p>}
    </div>
  );
}

function Chip({ on, onClick, children }: { on: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button type="button" onClick={onClick}
      className={`rounded-md px-2.5 py-1 text-xs font-medium ${
        on ? "bg-accent text-accent-on" : "border border-edge text-ink-muted hover:bg-fog-800"
      }`}>
      {children}
    </button>
  );
}
