// An app's icon, the way a store shows one: a rounded square at a fixed
// size, the same on a card, a table row, and the top of a listing. Two
// kinds of artwork sit in it. A logo is the project's mark on a transparent
// ground, set inside Fog's own tile with a margin so a round mark and a
// square one read as the same object; a tile (named tile.svg) is an icon
// drawn edge to edge, shown as is. An entry with no artwork — a spec that
// never came from the catalog — falls back to its emoji, so the shape on
// the page never changes.

import type { CatalogEntry } from "../api";

type Iconed = Pick<CatalogEntry, "logo" | "icon">;

export default function AppIcon({
  entry,
  size = 56,
  className = "",
}: {
  entry?: Iconed | null;
  size?: number;
  className?: string;
}) {
  const logo = entry?.logo;
  const tile = !!logo && /\/tile\.\w+$/.test(logo);
  return (
    <span
      aria-hidden
      className={`inline-flex shrink-0 select-none items-center justify-center overflow-hidden rounded-[22%] ${
        tile ? "" : "border border-veil-edge bg-fog-800"
      } ${className}`}
      style={{ width: size, height: size }}
    >
      {logo ? (
        <img
          src={logo}
          alt=""
          draggable={false}
          loading="lazy"
          className={tile ? "h-full w-full" : "h-[66%] w-[66%] object-contain"}
        />
      ) : (
        <span style={{ fontSize: Math.round(size * 0.5), lineHeight: 1 }}>{entry?.icon ?? "📦"}</span>
      )}
    </span>
  );
}
