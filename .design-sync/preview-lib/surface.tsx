// Shared wrapper the authored previews (.design-sync/previews/) import.
// The card harness paints a white page; fog's components sit on the page
// ground fog gives <body> (bg-fog-950 text-fog-100 — see frontend/src/index.css),
// so every cell renders on that ground, the way the app does.
import type { ReactNode } from "react";

export function Surface({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <div className={`rounded-lg bg-fog-950 p-4 text-fog-100 antialiased ${className}`}>{children}</div>;
}
