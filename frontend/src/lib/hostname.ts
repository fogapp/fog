// The one place the frontend applies Fog's hostname rule. Mirrors
// apps.RouteHost in internal/apps/spec.go: a route's explicit host wins, a
// dotless one is a subdomain label under the base domain, and with no host at
// all the app is served at <name>.<base domain>.

/** The hostname a route is served on, or null while the base domain is unknown. */
export function routeHost(appName: string, host: string | undefined, baseDomain: string | undefined): string | null {
  if (host?.includes(".")) return host;
  const label = host || appName;
  if (!baseDomain) return null;
  return `${label}.${baseDomain}`;
}

/** Mirrors the backend's route-host check for instant inline feedback. */
export function validateSubdomain(label: string): string | null {
  const s = label.trim();
  if (!s) return null; // empty means "use the app name" — the default, not an error
  if (s.includes(".")) return "One label only — a dot would put the app outside the wildcard certificate Fog issues for your domain.";
  if (s.length > 63) return "Too long — a hostname label maxes out at 63 characters.";
  if (!/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(s))
    return "Letters, digits, and hyphens only, starting and ending with a letter or digit.";
  return null;
}

// Names Fog serves itself; an app answering on one of them would shadow the
// Fog UI or its proxy.
const RESERVED = new Set(["fog", "caddy", "postgres", "fog-postgres"]);

export function reservedSubdomain(label: string): boolean {
  return RESERVED.has(label.trim().toLowerCase());
}
