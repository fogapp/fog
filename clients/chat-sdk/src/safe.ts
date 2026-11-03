// What a UI may do with bytes and links a sender chose. A sender controls an
// attachment's bytes and its stated type, and every URL in a message; none of
// it may run, or be fetched by a reader's browser, in fog's origin.

// The image types a UI may show inline, as an <img>. SVG is not one: it is a
// document that can carry script, and it is never given a type that lets a
// browser treat it as one.
export type InlineImageType = "image/png" | "image/jpeg" | "image/gif" | "image/webp";

// inlineImageType reads what decrypted bytes are, from their first bytes,
// ignoring the type the sender stated, and answers an inert image type to
// show them as, or null: anything else is only ever downloaded.
export function inlineImageType(bytes: Uint8Array): InlineImageType | null {
  const at = (i: number, ...b: number[]) => b.every((x, j) => bytes[i + j] === x);
  if (bytes.length >= 8 && at(0, 0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a)) return "image/png";
  if (bytes.length >= 3 && at(0, 0xff, 0xd8, 0xff)) return "image/jpeg";
  if (bytes.length >= 6 && (at(0, 0x47, 0x49, 0x46, 0x38, 0x37, 0x61) || at(0, 0x47, 0x49, 0x46, 0x38, 0x39, 0x61))) return "image/gif";
  if (bytes.length >= 12 && at(0, 0x52, 0x49, 0x46, 0x46) && at(8, 0x57, 0x45, 0x42, 0x50)) return "image/webp";
  return null;
}

// DOWNLOAD_TYPE is the only type a downloaded attachment's blob carries:
// whatever it is, the browser saves it rather than opens it.
export const DOWNLOAD_TYPE = "application/octet-stream";

// safeHref answers a link a reader may follow — http, https or mailto,
// absolute — or undefined, for anything else (javascript:, data:, blob:,
// file:, relative paths into fog itself).
export function safeHref(raw: string | undefined | null): string | undefined {
  if (!raw) return undefined;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return undefined;
  }
  if (u.protocol !== "http:" && u.protocol !== "https:" && u.protocol !== "mailto:") return undefined;
  if ((u.protocol === "http:" || u.protocol === "https:") && !u.hostname) return undefined;
  return u.href;
}

// isBlobRef says a string is what an attachment names its blob by: the hex
// sha256 of the ciphertext, nothing else. Content naming anything else is
// dropped at decode, and the API refuses to fetch it.
export function isBlobRef(v: unknown): v is string {
  return typeof v === "string" && /^[0-9a-f]{64}$/.test(v);
}

// systemLabel says who a line stamped by fog's bot speaks for: fog itself,
// or the app, webhook or MCP client that posted through it. undefined for
// anything else, which no client renders as fog's.
export function systemLabel(system: string | undefined): { name: string; via: "fog" | "app" | "webhook" | "mcp" } | undefined {
  if (system === "fog") return { name: "fog", via: "fog" };
  const m = /^(app|webhook|mcp):([^\u0000-\u001f\u007f]{1,100})$/u.exec(system ?? "");
  return m ? { name: m[2], via: m[1] as "app" | "webhook" | "mcp" } : undefined;
}

// linkHost is the host a link card names: the link's own, never the
// card's "site", which is the sender's say. "" for a link that isn't one.
export function linkHost(url: string): string {
  const href = safeHref(url);
  if (!href || href.startsWith("mailto:")) return "";
  try {
    return new URL(href).hostname;
  } catch {
    return "";
  }
}
