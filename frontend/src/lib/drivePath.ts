// Drive paths are slash-separated and relative to the Drive's root, with ""
// for the root itself.

// The folder holding p: "" for an entry at the root.
export function parentOf(p: string): string {
  return p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
}

// p's last segment.
export function baseOf(p: string): string {
  return p.includes("/") ? p.slice(p.lastIndexOf("/") + 1) : p;
}
