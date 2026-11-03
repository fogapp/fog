// Where a sign-in goes next. The gate and fog's sign-in flow send people to
// /login or /confirm with ?next=; afterwards they return there — but only
// to fog itself or a sibling host on the household's domain over https, so
// the parameter is never an open redirect.

function safeNext(next: string | null, here: URL): string | null {
  if (!next) return null;
  // A browser drops tabs and newlines from a URL and reads a backslash as a
  // slash; refuse them outright rather than lean on how the parser below
  // happens to normalise "/<tab>/evil.example".
  if (/[\u0000-\u001f\u007f\\]/.test(next)) return null;
  try {
    const url = new URL(next, here.origin);
    if (url.username || url.password) return null;
    if (url.origin === here.origin) return url.protocol === "http:" || url.protocol === "https:" ? url.toString() : null;
    // fog lives at fog.<domain>, and its siblings at <app>.<domain>. Only
    // there: fog reached by any other name (a tunnel's, a dynamic-DNS
    // host's) has no household domain, and "siblings" of it would be
    // strangers' hosts.
    if (!here.hostname.startsWith("fog.")) return null;
    const domain = here.hostname.slice("fog.".length);
    if (domain.includes(".") && url.protocol === "https:" && url.hostname.endsWith("." + domain) && url.port === "") return url.toString();
  } catch { /* not a URL: nowhere to go */ }
  return null;
}

// followNext leaves for next when there is a safe one; the return says
// whether it did.
export function followNext(): boolean {
  const next = safeNext(new URLSearchParams(window.location.search).get("next"), new URL(window.location.href));
  if (!next) return false;
  window.location.href = next;
  return true;
}
