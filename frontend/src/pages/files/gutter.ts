// Drive's inset: half the page gutter. Drive is a file browser, and the
// columns want the width — so its tabs, toolbar, trail, and list all sit
// halfway between the sidebar and the page gutter the header keeps, on one
// edge with each other.
export const DRIVE_GUTTER = "px-2 sm:px-3 lg:px-4";

// The same inset in pixels, for layout that has to count it (the grid's
// column math). Tailwind's sm and lg breakpoints.
export function driveGutterPx(): number {
  if (typeof window === "undefined") return 8;
  return window.innerWidth >= 1024 ? 16 : window.innerWidth >= 640 ? 12 : 8;
}
