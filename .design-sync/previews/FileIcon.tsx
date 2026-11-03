import { FileIcon } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

const kinds = ["folder", "apps", "image", "video", "audio", "pdf", "text", "code", "archive", "document", "link", "file"] as const;

// One glyph per category, each in its own tone.
export function AllKinds() {
  return (
    <Surface className="grid grid-cols-4 gap-4">
      {kinds.map((k) => (
        <div key={k} className="flex items-center gap-2 text-xs text-ink-muted">
          <FileIcon category={k} />
          {k}
        </div>
      ))}
    </Surface>
  );
}

export function Sizes() {
  return (
    <Surface className="flex items-end gap-6">
      <FileIcon category="image" className="h-4 w-4" />
      <FileIcon category="image" />
      <FileIcon category="image" className="h-8 w-8" />
      <FileIcon category="image" className="h-12 w-12" />
    </Surface>
  );
}

// The row it lives in on the Drive page.
export function InAFileRow() {
  return (
    <Surface className="max-w-md divide-y divide-fog-800">
      {[
        ["folder", "Photos", "312 items"],
        ["pdf", "lease-2026.pdf", "1.1 MB"],
        ["code", "fog.yaml", "2 KB"],
        ["archive", "backup-kit.tar.zst", "18 GB"],
      ].map(([k, name, meta]) => (
        <div key={name} className="flex items-center gap-3 py-2 text-sm">
          <FileIcon category={k as (typeof kinds)[number]} />
          <span className="flex-1 text-ink">{name}</span>
          <span className="text-xs text-ink-muted">{meta}</span>
        </div>
      ))}
    </Surface>
  );
}
