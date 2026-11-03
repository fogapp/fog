// A Drive name made safe to show. Names are whatever an app wrote, and some
// characters in them lie about what a file is: control characters (shown as
// their Control Pictures symbol, ␊ ␛ ␡, or \xNN for C1); bidirectional
// controls (U+202A–202E, U+2066–2069, LRM, RLM, ALM), which reorder what
// follows so "invoice\u202Efdp.exe" reads "invoiceexe.pdf"; the line and
// paragraph separators (U+2028, U+2029); and characters that are not drawn
// (U+200B, U+2060–2064, U+FEFF, U+180E). Each of the last three kinds is
// shown as ⟨U+202E⟩ and does nothing. The zero-width joiner and non-joiner
// (U+200D, U+200C) are kept between two non-ASCII characters, where emoji
// sequences and scripts such as Persian need them, and shown elsewhere. For
// display only: every operation keeps the real name. The server's twin is
// files.DisplayName.

const BIDI = /[\u202A-\u202E\u2066-\u2069\u200E\u200F\u061C]/;
const INVISIBLE = /[\u200B\u2060-\u2064\uFEFF\u180E\u2028\u2029]/;
const JOINER = /[\u200C\u200D]/;
const UNSAFE = /[\u0000-\u001F\u007F-\u009F\u202A-\u202E\u2066-\u2069\u200E\u200F\u061C\u200B-\u200D\u2060-\u2064\uFEFF\u180E\u2028\u2029]/;

function hex(cp: number, width: number): string {
  return cp.toString(16).toUpperCase().padStart(width, "0");
}

function unsafeChar(ch: string | undefined): boolean {
  return ch === undefined || UNSAFE.test(ch);
}

export function displayName(name: string, opts: { keepNewlines?: boolean } = {}): string {
  if (!UNSAFE.test(name)) return name;
  const chars = Array.from(name);
  let out = "";
  chars.forEach((ch, i) => {
    const cp = ch.codePointAt(0)!;
    const prev = chars[i - 1];
    const next = chars[i + 1];
    if (opts.keepNewlines && (ch === "\n" || ch === "\t")) out += ch;
    else if (
      JOINER.test(ch) &&
      prev !== undefined && next !== undefined &&
      prev.codePointAt(0)! >= 0x80 && next.codePointAt(0)! >= 0x80 &&
      !unsafeChar(prev) && !unsafeChar(next)
    )
      out += ch;
    else if (cp < 0x20) out += String.fromCodePoint(0x2400 + cp);
    else if (cp === 0x7f) out += "␡";
    else if (cp >= 0x80 && cp <= 0x9f) out += `\\x${hex(cp, 2)}`;
    else if (BIDI.test(ch) || INVISIBLE.test(ch) || JOINER.test(ch)) out += `⟨U+${hex(cp, 4)}⟩`;
    else out += ch;
  });
  return out;
}

// FileName shows a name isolated from the text around it, so even a
// right-to-left name cannot reorder its neighbours.
export function FileName({ name, className }: { name: string; className?: string }) {
  return <bdi className={className}>{displayName(name)}</bdi>;
}
