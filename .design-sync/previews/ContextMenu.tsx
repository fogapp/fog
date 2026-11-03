import { ContextMenu, FileIcon } from "fog-frontend";

// A right-click menu is fixed at the pointer, so the card renders one open
// menu (cardMode: single) over a page-sized surface — the file row it was
// opened on. x/y are viewport coordinates.
export function OnAFile() {
  return (
    <div className="relative h-80 bg-fog-950 p-4 text-ink antialiased">
      <div className="flex items-center gap-3 rounded-md bg-fog-900 px-3 py-2 text-sm">
        <FileIcon category="video" />
        <span className="flex-1 text-ink">holiday-2026.mkv</span>
        <span className="text-xs text-ink-muted">4.2 GB</span>
      </div>
      <div className="flex items-center gap-3 px-3 py-2 text-sm">
        <FileIcon category="folder" />
        <span className="flex-1 text-ink">Photos</span>
        <span className="text-xs text-ink-muted">312 items</span>
      </div>
      <ContextMenu
        x={180}
        y={44}
        onClose={() => {}}
        items={[
          { label: "Open", shortcut: "↵" },
          { label: "Download" },
          { separator: true },
          { label: "Rename", shortcut: "F2" },
          { label: "Move to…" },
          { label: "Share with a peer", disabled: true },
          { separator: true },
          { label: "Move to trash", danger: true, shortcut: "⌫" },
        ]}
      />
    </div>
  );
}
