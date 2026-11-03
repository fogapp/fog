// Drive's toolbar: search, how the listing looks, which point in time, and
// the verbs that add to a folder or act on the selection.

import { Link } from "react-router-dom";
import type { FileSnapshot, FilesSort } from "../../api";
import { Spinner } from "../../components/Async";
import { paths } from "../../lib/paths";
import { tool } from "./DriveChrome";
import { DRIVE_GUTTER } from "./gutter";
import SnapshotMenu from "./SnapshotMenu";
import type { useViewPrefs } from "./viewPrefs";
import { control } from "../../components/Field";

const SORTS: { id: FilesSort; label: string }[] = [
  { id: "name", label: "Name" },
  { id: "mtime", label: "Date modified" },
  { id: "size", label: "Size" },
  { id: "kind", label: "Kind" },
];

export default function DriveToolbar({
  q, onQuery, rel, at, prefs, onPickSnapshot, onSnapshotNow, canAdd, onNewFolder, onUpload, onUploadFolder,
  downloadHref, selected, onTrash, fetching, walking, trashItems, onRefresh,
}: {
  q: string;
  onQuery: (q: string) => void;
  rel: string;
  at?: number;
  prefs: ReturnType<typeof useViewPrefs>;
  onPickSnapshot: (s: FileSnapshot | null) => void;
  onSnapshotNow: () => void;
  // Off while searching, in a snapshot, or in a read-only copy.
  canAdd: boolean;
  onNewFolder: () => void;
  onUpload: () => void;
  onUploadFolder: () => void;
  downloadHref?: string;
  selected: number;
  // Absent where nothing can be trashed.
  onTrash?: () => void;
  fetching: boolean;
  walking: boolean;
  trashItems: number;
  onRefresh: () => void;
}) {
  return (
    <div className={`flex flex-wrap items-center gap-2 border-b border-hair py-3 ${DRIVE_GUTTER}`}>
      <input
        value={q}
        onChange={(e) => onQuery(e.target.value)}
        onKeyDown={(e) => e.key === "Escape" && onQuery("")}
        placeholder={rel ? `Search in ${rel.slice(rel.lastIndexOf("/") + 1)}…` : "Search names…"}
        aria-label="Search file names"
        disabled={at !== undefined}
        className={`${control} w-56 disabled:opacity-40`}
      />
      <div className="flex overflow-hidden rounded-md border border-edge" role="group" aria-label="View">
        {(["list", "grid"] as const).map((v) => (
          <button
            key={v}
            type="button"
            onClick={() => prefs.setView(v)}
            aria-pressed={prefs.view === v}
            title={v === "list" ? "List" : "Grid"}
            className={`px-2.5 py-1.5 text-xs font-medium ${prefs.view === v ? "bg-fog-700 text-ink" : "text-ink-muted hover:bg-fog-800"}`}
          >
            {v === "list" ? "≡" : "▦"}
          </button>
        ))}
      </div>
      <label className="flex items-center gap-1 text-xs text-ink-muted">
        Sort
        <select
          value={prefs.sort}
          onChange={(e) => prefs.setSort(e.target.value as FilesSort, prefs.desc)}
          className={control}
        >
          {SORTS.map((s) => (
            <option key={s.id} value={s.id}>{s.label}</option>
          ))}
        </select>
      </label>
      <button type="button" onClick={() => prefs.setSort(prefs.sort, !prefs.desc)} className={tool} title={prefs.desc ? "Descending" : "Ascending"}>
        {prefs.desc ? "▼" : "▲"}
      </button>
      <SnapshotMenu at={at} onPick={onPickSnapshot} onSnapshotNow={onSnapshotNow} className={tool} />
      <span className="mx-1 h-5 w-px bg-fog-800" />
      <button type="button" onClick={onNewFolder} disabled={!canAdd} className={tool}>
        New folder
      </button>
      <button type="button" onClick={onUpload} disabled={!canAdd} className={tool}>
        Upload
      </button>
      <button type="button" onClick={onUploadFolder} disabled={!canAdd} className={tool} title="Upload a whole folder">
        Upload folder
      </button>
      <a href={downloadHref} aria-disabled={!downloadHref} className={`${tool} ${downloadHref ? "" : "pointer-events-none opacity-40"}`}>
        Download{selected > 1 ? ` ${selected}` : ""}
      </a>
      <button type="button" onClick={onTrash} disabled={selected === 0 || !onTrash} className={tool}>
        Trash{selected > 1 ? ` ${selected}` : ""}
      </button>
      <span className="ml-auto flex items-center gap-3 text-xs text-ink-muted">
        {fetching && <Spinner size="h-3 w-3" tone="border-fog-500" />}
        {walking && <span title="Fog is re-indexing the tree in the background.">indexing…</span>}
        <Link to={paths.trash} className="hover:text-ink">
          Trash{trashItems > 0 ? ` (${trashItems})` : ""}
        </Link>
        <button type="button" onClick={onRefresh} className="px-1 hover:text-ink" title="Refresh">↻</button>
      </span>
    </div>
  );
}
