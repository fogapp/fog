// What this browser saw lately, kept for the feedback dialog. By the time
// someone reaches for the bug button the thing that surprised them has
// usually scrolled out of view: the console error, the request that came
// back 500, the page they were on before this one. The trail keeps the
// last few of each in memory — never persisted, never sent anywhere until a
// person presses Send on a report — and snapshot() is what that report
// attaches.

export interface TrailEvent {
  at: string;
  kind: "error" | "rejection" | "console" | "request" | "nav";
  message: string;
  detail?: string;
}

export interface ClientSnapshot {
  capturedAt: string;
  url: string;
  title: string;
  userAgent: string;
  language: string;
  timeZone: string;
  viewport: { width: number; height: number; pixelRatio: number };
  theme: string;
  online: boolean;
  loadedAt: string;
  // Newest last, per kind so a burst of one can't push out the others.
  errors: TrailEvent[];
  requests: TrailEvent[];
  navigation: TrailEvent[];
}

const KEEP = { errors: 40, requests: 40, navigation: 20 };
const MAX_TEXT = 600;

const buckets: Record<keyof typeof KEEP, TrailEvent[]> = { errors: [], requests: [], navigation: [] };
const loadedAt = new Date().toISOString();
let installed = false;

function push(bucket: keyof typeof KEEP, e: Omit<TrailEvent, "at">) {
  const list = buckets[bucket];
  list.push({ at: new Date().toISOString(), ...e, message: clip(e.message), detail: e.detail && clip(e.detail) });
  if (list.length > KEEP[bucket]) list.splice(0, list.length - KEEP[bucket]);
}

function clip(s: string): string {
  return s.length > MAX_TEXT ? s.slice(0, MAX_TEXT) + "…" : s;
}

function describe(v: unknown): string {
  if (v instanceof Error) return `${v.name}: ${v.message}`;
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

// Only same-origin paths are recorded: a request to anywhere else is not
// fog's to report, and its URL may carry someone else's token.
function sameOriginPath(input: RequestInfo | URL): string | null {
  try {
    const raw = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    const u = new URL(raw, window.location.href);
    return u.origin === window.location.origin ? u.pathname + u.search : null;
  } catch {
    return null;
  }
}

export function installTrail() {
  if (installed || typeof window === "undefined") return;
  installed = true;

  window.addEventListener("error", (e) => {
    const where = e.filename ? `${e.filename}:${e.lineno}:${e.colno}` : undefined;
    push("errors", { kind: "error", message: e.message || describe(e.error), detail: e.error?.stack ?? where });
  });
  window.addEventListener("unhandledrejection", (e) => {
    push("errors", { kind: "rejection", message: describe(e.reason), detail: e.reason?.stack });
  });

  for (const level of ["error", "warn"] as const) {
    const original = console[level].bind(console);
    console[level] = (...args: unknown[]) => {
      push("errors", { kind: "console", message: `console.${level}: ${args.map(describe).join(" ")}` });
      original(...args);
    };
  }

  const originalFetch = window.fetch.bind(window);
  window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = sameOriginPath(input);
    const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
    const started = performance.now();
    try {
      const res = await originalFetch(input, init);
      if (path) {
        const ms = Math.round(performance.now() - started);
        const event = { kind: "request" as const, message: `${method} ${path} → ${res.status} (${ms}ms)` };
        if (!res.ok) {
          // The error body is what the page showed, when it showed anything.
          res.clone().text().then(
            (text) => push("requests", { ...event, detail: text }),
            () => push("requests", event),
          );
        } else {
          push("requests", event);
        }
      }
      return res;
    } catch (err) {
      if (path) push("requests", { kind: "request", message: `${method} ${path} → failed`, detail: describe(err) });
      throw err;
    }
  };
}

// Called by the shell on every route change.
export function recordNavigation(path: string) {
  const last = buckets.navigation[buckets.navigation.length - 1];
  if (last?.message === path) return;
  push("navigation", { kind: "nav", message: path });
}

export function snapshot(): ClientSnapshot {
  let timeZone = "";
  try {
    timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch {
    /* an old engine; the zone is a nicety */
  }
  const root = document.documentElement;
  return {
    capturedAt: new Date().toISOString(),
    url: window.location.pathname + window.location.search + window.location.hash,
    title: document.title,
    userAgent: navigator.userAgent,
    language: navigator.language,
    timeZone,
    viewport: { width: window.innerWidth, height: window.innerHeight, pixelRatio: window.devicePixelRatio },
    theme: root.dataset.theme ?? (window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "system dark" : "system light"),
    online: navigator.onLine,
    loadedAt,
    errors: [...buckets.errors],
    requests: [...buckets.requests],
    navigation: [...buckets.navigation],
  };
}
