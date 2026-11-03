// Human-readable byte sizes, shared by every place that quotes storage.
// plural: "1 file", "12 files".
export function plural(n: number, word: string): string {
  return `${n.toLocaleString()} ${word}${n === 1 ? "" : "s"}`;
}

export function formatBytes(n: number): string {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(n >= 10 || i === 0 ? 0 : 1)} ${units[i]}`;
}

// Whole-disk capacities, decimal units — a "500 GB" drive is 500e9 bytes, and
// the installer's disk picker must agree with the label on the hardware, not
// call it "466 GB". Everything that isn't a raw disk uses formatBytes above.
export function formatDiskSize(bytes: number): string {
  const gb = bytes / 1e9;
  return gb >= 1000 ? `${(gb / 1000).toFixed(1)} TB` : `${Math.round(gb)} GB`;
}

// Compact relative timestamps for "last seen" columns. Falls back to the
// locale date once "ago" stops being the useful framing.
export function timeAgo(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  if (s < 7 * 86400) return `${Math.floor(s / 86400)}d ago`;
  return new Date(iso).toLocaleDateString();
}

// timeUntil: timeAgo's mirror, for deadlines rather than history. timeAgo
// clamps the future to zero — an invite with 23 hours left came out as
// "just now" — so anything counting down needs this instead.
export function timeUntil(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const s = (t - Date.now()) / 1000;
  if (s <= 0) return "expired";
  if (s < 60) return "in under a minute";
  if (s < 3600) return `in ${Math.floor(s / 60)}m`;
  if (s < 86400) return `in ${Math.floor(s / 3600)}h`;
  return `in ${Math.floor(s / 86400)}d`;
}

// What to call a federated peer on screen: its local label if one is set,
// otherwise the name it advertised when this instance paired with it.
export function peerLabel(peer: { name: string; label?: string }): string {
  return peer.label?.trim() || peer.name;
}

// How a fog version reads on screen: the number alone. The wire carries the
// git tag ("v0.6.14"), but every place that shows it already says which
// instance it belongs to, so the "v" is one character of noise.
export function versionLabel(v: string): string {
  return v.replace(/^v/, "");
}

// A file's modification time the way a file manager shows it: the time if
// it was today, the day if it was this year, the full date otherwise.
export function formatWhen(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const now = new Date();
  if (d.toDateString() === now.toDateString()) {
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  if (d.getFullYear() === now.getFullYear()) {
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }
  return d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

// uaSummary boils a User-Agent down to the words a person recognises:
// browser and platform, or the app name a native client sent.
export function uaSummary(ua: string | undefined): string {
  if (!ua) return "";
  const browser =
    /Edg\//.test(ua) ? "Edge" :
    /OPR\//.test(ua) ? "Opera" :
    /Firefox\//.test(ua) ? "Firefox" :
    /Chrome\//.test(ua) ? "Chrome" :
    /Safari\//.test(ua) && /Version\//.test(ua) ? "Safari" : "";
  const os =
    /iPhone|iPad/.test(ua) ? "iOS" :
    /Android/.test(ua) ? "Android" :
    /Mac OS X/.test(ua) ? "macOS" :
    /Windows/.test(ua) ? "Windows" :
    /CrOS/.test(ua) ? "ChromeOS" :
    /Linux/.test(ua) ? "Linux" : "";
  const parts = [browser, os].filter(Boolean);
  if (parts.length) return parts.join(" on ");
  // Not a browser: keep the first token (a native app's product name).
  return ua.split(/[\s/(]/)[0]?.slice(0, 32) ?? "";
}
