// Turning a catalog entry plus the operator's answers into the request
// that creates the app. Pure functions, shared by the install panel and
// by companion installs, which take the defaults without a form.

import type { AppSpec, CatalogEntry, Volume } from "../../api";
import { ackKey, defaultPlacementFor, planMounts } from "../../components/MountPlan";
import { validateSubdomain } from "../../lib/hostname";

// generateValue produces fresh random secrets client-side. alnum32 matches
// `rails db:encryption:init`; hex64 matches `openssl rand -hex 32` (a 32-byte
// AES-256 key); hex128 matches `rails secret`.
export function generateValue(kind: string): string {
  const raw = new Uint8Array(64);
  crypto.getRandomValues(raw);
  const hex = (n: number) => [...raw.slice(0, n)].map((b) => b.toString(16).padStart(2, "0")).join("");
  if (kind === "hex32") return hex(16);
  if (kind === "hex64") return hex(32);
  if (kind === "hex128") return hex(64);
  const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  return [...raw].map((b) => chars[b % chars.length]).join("").slice(0, 32);
}

// defaultValues fills an entry's options without operator input: generated
// secrets, declared defaults, browser timezone. Where folders go is not an
// option value — it is the mount plan, which defaults on its own from the
// app's name. Companions install this way.
export function defaultValues(entry: CatalogEntry, browserTZ: string): Record<string, string> {
  const v: Record<string, string> = {};
  for (const o of entry.options ?? []) {
    if (o.target.startsWith("volume:")) continue;
    if (o.generate) v[o.key] = generateValue(o.generate);
    else if (o.key === "tz") v[o.key] = browserTZ;
    else v[o.key] = o.default ?? "";
  }
  return v;
}

// fingerprintOf mirrors the contract's secrets_fingerprint form: "sha256:"
// plus the first 12 hex characters of the value's digest. Computed in the
// browser so a pasted secret is checked against the artifact without being
// sent anywhere to be checked.
export async function fingerprintOf(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  const hex = [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
  return `sha256:${hex.slice(0, 12)}`;
}

// buildInstallPayload applies option values onto a copy of the template spec.
// subdomain, when given, overrides the <app name> half of every route's
// hostname; left empty the routes stay hostless and keep tracking the app
// name and base domain, exactly as they did before this input existed.
export function buildInstallPayload(
  entry: CatalogEntry, values: Record<string, string>, placements: Record<string, string>,
  appName: string, subdomain = "",
) {
  const spec: AppSpec = JSON.parse(JSON.stringify(entry.spec));
  const secrets: Record<string, string> = {};
  const label = subdomain.trim().toLowerCase();
  if (label) {
    if (validateSubdomain(label)) throw new Error(`Address: ${validateSubdomain(label)}`);
    // Only routes that don't already name a host — an entry that pins one has
    // a reason to, and the operator didn't ask to override that.
    spec.routes = (spec.routes ?? []).map((r) => (r.host ? r : { ...r, host: label }));
  }
  for (const o of entry.options ?? []) {
    const val = values[o.key]?.trim() ?? "";
    if (o.required && !val) throw new Error(`${o.label} is required`);
    if (o.target.startsWith("env:")) {
      if (val) spec.env = { ...spec.env, [o.target.slice(4)]: val };
    } else if (o.target.startsWith("volume:")) {
      // Folders are not option values; the mount plan below places them all.
    } else if (o.target.startsWith("secret:")) {
      const key = o.target.slice(7);
      if (val) secrets[key] = val;
      // An optional secret nobody filled in stays undeclared — a declared
      // secret with no value fails every deploy. It can be added later from
      // the app's Config tab if it turns out to be needed.
      else spec.secrets = (spec.secrets ?? []).filter((k) => k !== key);
    } else if (o.target === "repo") {
      if (val) spec.source = { ...spec.source, repo: val };
    }
  }

  // Every folder the app mounts, placed in one pass — the plan the pane
  // showed, applied. A volume the operator never touched still travels
  // explicitly, so what was on screen is what gets installed rather than
  // something the server fills in afterwards.
  for (const m of planMounts(entry).rows) {
    const val = (placements[m.key] ?? defaultPlacementFor(m, appName)).trim();
    const label = m.option?.label ?? m.volume.name;
    const place = (v: Volume): Volume =>
      val.startsWith("files:") ? { ...v, files: val.slice(6).replace(/^\/+|\/+$/g, ""), host: undefined }
      : val.startsWith("host:") ? { ...v, host: val.slice(5), files: undefined }
      : { ...v, files: undefined, host: undefined };
    if (val.startsWith("files:") && !val.slice(6).replace(/\//g, "")) throw new Error(`${label}: name a folder in Drive`);
    if (val.startsWith("host:") && !val.slice(5).startsWith("/")) throw new Error(`${label}: a host path must be absolute`);
    // A host folder outside fog's storage the operator acknowledged in the
    // pane: said in the spec, as the server requires.
    if (val.startsWith("host:") && placements[ackKey(m.key)] === "1") {
      spec.acknowledgeHostAccess = [...(spec.acknowledgeHostAccess ?? []), val.slice(5)];
    }
    if (m.service) {
      spec.aux = spec.aux?.map((a) =>
        a.name === m.service ? { ...a, volumes: a.volumes?.map((v) => (v.name === m.volume.name ? place(v) : v)) } : a,
      );
    } else {
      spec.volumes = (spec.volumes ?? []).map((v) => (v.name === m.volume.name ? place(v) : v));
    }
  }
  return { spec, secrets };
}

// parseDotenv reads a .env file the way the shells and dotenv loaders that
// write them do: KEY=value lines, an optional `export`, # comments, single
// quotes taken literally, double quotes with \n \" \\ escapes and allowed to
// span lines. Read in the browser — the file itself goes nowhere.
export function parseDotenv(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  for (let i = 0; i < lines.length; i++) {
    const m = /^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*=\s*(.*)$/.exec(lines[i]);
    if (!m) continue;
    let rest = m[2];
    let val: string;
    if (rest.startsWith("'")) {
      const end = rest.indexOf("'", 1);
      val = end < 0 ? rest.slice(1) : rest.slice(1, end);
    } else if (rest.startsWith('"')) {
      rest = rest.slice(1);
      let end = closingQuote(rest);
      while (end < 0 && i + 1 < lines.length) {
        rest += "\n" + lines[++i];
        end = closingQuote(rest);
      }
      val = (end < 0 ? rest : rest.slice(0, end)).replace(/\\([n"\\])/g, (_, c) => (c === "n" ? "\n" : c));
    } else {
      val = rest.replace(/\s+#.*$/, "").trim();
    }
    out[m[1]] = val;
  }
  return out;
}

function closingQuote(s: string): number {
  for (let i = 0; i < s.length; i++) {
    if (s[i] === "\\") i++;
    else if (s[i] === '"') return i;
  }
  return -1;
}

// valuesFromDotenv maps a parsed .env onto an entry's options by the
// variable each one sets, returning the updated values and the option keys
// it filled. Variables no option sets are left out — they are for the app's
// Config tab, not an install form.
export function valuesFromDotenv(
  entry: CatalogEntry, values: Record<string, string>, env: Record<string, string>,
): { values: Record<string, string>; filled: string[] } {
  const next = { ...values };
  const filled: string[] = [];
  for (const o of entry.options ?? []) {
    const m = /^(?:env|secret):(.+)$/.exec(o.target);
    if (m && env[m[1]] !== undefined && env[m[1]] !== "") {
      next[o.key] = env[m[1]];
      filled.push(o.key);
    }
  }
  return { values: next, filled };
}
