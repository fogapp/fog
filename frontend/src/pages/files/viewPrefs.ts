// How you last looked at a folder — list or grid, sorted by what — is
// remembered per folder in this browser, the way Finder remembers a
// window. The view is global (people pick one and stay), the sort is per
// folder (Photos by date, Documents by name).

import { useCallback, useEffect, useState } from "react";
import type { FilesSort } from "../../api";

export type FilesView = "list" | "grid";

interface Prefs {
  view: FilesView;
  sorts: Record<string, { sort: FilesSort; desc: boolean }>;
}

const KEY = "fog.files.prefs";
const MAX_REMEMBERED = 200;

function load(): Prefs {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw) {
      const p = JSON.parse(raw) as Partial<Prefs>;
      return { view: p.view === "grid" ? "grid" : "list", sorts: p.sorts ?? {} };
    }
  } catch {
    /* private mode, or garbage */
  }
  return { view: "list", sorts: {} };
}

function save(p: Prefs) {
  try {
    const keys = Object.keys(p.sorts);
    if (keys.length > MAX_REMEMBERED) for (const k of keys.slice(0, keys.length - MAX_REMEMBERED)) delete p.sorts[k];
    localStorage.setItem(KEY, JSON.stringify(p));
  } catch {
    /* private mode */
  }
}

export function useViewPrefs(path: string) {
  const [prefs, setPrefs] = useState<Prefs>(load);
  useEffect(() => save(prefs), [prefs]);
  const current = prefs.sorts[path] ?? { sort: "name" as FilesSort, desc: false };
  const setView = useCallback((view: FilesView) => setPrefs((p) => ({ ...p, view })), []);
  const setSort = useCallback(
    (sort: FilesSort, desc: boolean) => setPrefs((p) => ({ ...p, sorts: { ...p.sorts, [path]: { sort, desc } } })),
    [path],
  );
  return { view: prefs.view, sort: current.sort, desc: current.desc, setView, setSort };
}
