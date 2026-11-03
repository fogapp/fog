// The one query for GET /api/capacity, and the words for its numbers. The
// server decides whether an app fits; this file only says so in
// sentences, so a card, a listing and a table row all tell the same story
// in the same voice.

import { useQuery } from "@tanstack/react-query";
import { get, type Capacity, type CatalogEntry, type Fit, type FitTone } from "../api";
import { formatBytes } from "./format";

export function useCapacity() {
  return useQuery({
    queryKey: ["capacity"],
    queryFn: () => get<Capacity>("/api/capacity"),
    refetchInterval: 15000,
  });
}

// The resources behind the fit's overall tone — the ones at that worst
// tone, so a verdict names what is short instead of leaving the reader to
// guess (a memory figure beside "too much" reads as memory, whatever the
// cause was).
function strained(fit: Fit): ("memory" | "processor" | "disk")[] {
  const out: ("memory" | "processor" | "disk")[] = [];
  if (fit.tone === "ok") return out;
  if (fit.memory.tone === fit.tone) out.push("memory");
  if (fit.cpu.tone === fit.tone) out.push("processor");
  if (fit.disk.tone === fit.tone) out.push("disk");
  return out;
}

function list(words: string[]): string {
  return words.length <= 1 ? words.join("") : `${words.slice(0, -1).join(", ")} and ${words[words.length - 1]}`;
}

const OVER_WORD = { memory: "memory", processor: "processor", disk: "disk space" } as const;

// The worst tone as a headline, naming what is short.
export function verdict(fit: Fit): string {
  const which = strained(fit);
  if (fit.tone === "over") return `Not enough ${list(which.map((r) => OVER_WORD[r]))}`;
  if (fit.tone === "tight") return `Getting tight on ${list(which)}`;
  return "Plenty of room";
}

export const TONE_TEXT: Record<FitTone, string> = {
  ok: "text-accent",
  tight: "text-warn",
  over: "text-warn",
};

// Rounded to what a person would say: "400 MB", "1 GB", "1.5 GB". A
// thousand megabytes is a gigabyte in conversation, whatever the binary
// prefix says.
export function approx(bytes: number): string {
  const gb = 1 << 30;
  if (bytes >= 1000 * (1 << 20)) return `${(bytes / gb).toFixed(1).replace(/\.0$/, "")} GB`;
  return formatBytes(bytes).replace(/\.0 /, " ");
}

function cpuPhrase(entry: CatalogEntry): string {
  const f = entry.footprint;
  if (!f) return "";
  if (f.cpu === "heavy") return f.cpuWhen ? `Heavy on the processor when ${f.cpuWhen}` : "Heavy on the processor";
  if (f.cpu === "moderate") return f.cpuWhen ? `Works the processor when ${f.cpuWhen}` : "Moderate on the processor";
  return "Light on the processor";
}

export interface FitLine {
  label: string;
  tone: FitTone;
  text: string;
}

// The three answers for a listing, each a sentence that stands alone.
// Where the host has no figures yet, the line says what the app costs
// and stops — a verdict needs a machine to measure against.
export function fitLines(entry: CatalogEntry, fit: Fit | undefined, cap: Capacity | undefined): FitLine[] {
  const f = entry.footprint;
  if (!f) return [];
  const host = cap?.host;
  const out: FitLine[] = [];

  // Memory
  {
    const cost = `About ${approx(f.memory)} at rest, up to ${approx(f.memoryPeak)} when busy`;
    if (!fit || !host || host.memTotal === 0) {
      out.push({ label: "Memory", tone: "ok", text: `${cost}.` });
    } else {
      const m = fit.memory;
      const pct = Math.round((m.after / host.memTotal) * 100);
      const text =
        m.tone === "over"
          ? `${cost} — even at rest that's past this machine's ${approx(host.memTotal)} (${approx(m.after)}). It would run from swap, and everything would slow down.`
          : m.tone === "tight"
            ? m.afterPeak > host.memTotal
              ? `${cost} — this machine would sit at ${pct}% of its ${approx(host.memTotal)}, and a busy spell (${approx(m.afterPeak)}) would spill into swap.`
              : `${cost} — this machine would sit at ${pct}% of its ${approx(host.memTotal)}. Apps start competing for memory around here.`
            : `${cost} — this machine would sit at ${pct}% of its ${approx(host.memTotal)}.`;
      out.push({ label: "Memory", tone: m.tone, text });
    }
  }

  // Processor
  {
    let text = cpuPhrase(entry) + ".";
    if (fit && host && host.cores > 0) {
      const c = fit.cpu;
      const cores = `${host.cores} ${host.cores === 1 ? "core" : "cores"}`;
      if (c.heavy.length > 0) {
        const who = c.heavy.length <= 3 ? c.heavy.join(", ") : `${c.heavy.slice(0, 2).join(", ")} and ${c.heavy.length - 2} more`;
        text +=
          c.tone !== "ok"
            ? ` Shares ${cores} with ${who}, which can each hold a core; busy at the same time, they take turns and run slower.`
            : ` ${who} ${c.heavy.length === 1 ? "is" : "are"} the other heavy ${c.heavy.length === 1 ? "app" : "apps"} on these ${cores}.`;
      } else if (c.tone !== "ok") {
        text += ` Busy all at once, the apps here would ask for more than the ${cores}; they take turns and run slower.`;
      } else {
        text += ` ${host.cores} ${host.cores === 1 ? "core" : "cores"}, nothing heavy installed yet.`;
      }
      out.push({ label: "Processor", tone: c.tone, text });
    } else {
      out.push({ label: "Processor", tone: "ok", text });
    }
  }

  // Disk
  {
    const grows = f.diskGrows ? ` Grows ${f.diskGrows}.` : "";
    if (!fit || !host || host.diskTotal === 0) {
      out.push({ label: "Disk", tone: "ok", text: `Needs ${approx(f.disk)} to download.${grows}` });
    } else {
      const d = fit.disk;
      const text =
        d.tone === "over"
          ? `Needs ${approx(f.disk)} to download — more than the ${approx(host.diskFree)} free on the data disk.${grows}`
          : d.tone === "tight"
            ? `Needs ${approx(f.disk)} to download, leaving only ${approx(d.freeAfter)} free on the data disk.${grows}`
            : `Needs ${approx(f.disk)} to download; ${approx(host.diskFree)} free on the data disk.${grows}`;
      out.push({ label: "Disk", tone: d.tone, text });
    }
  }
  return out;
}

// One short line for a card: the cost, and a warning when the fit is not
// plain. Empty for an entry with no footprint.
export function cardLine(entry: CatalogEntry, fit: Fit | undefined): { text: string; tone: FitTone } | null {
  const f = entry.footprint;
  if (!f) return null;
  const cost = `~${approx(f.memory)}`;
  const cpu = f.cpu === "heavy" ? "heavy" : f.cpu === "moderate" ? "moderate" : "light";
  if (!fit || fit.tone === "ok") return { text: `${cost} · ${cpu} on the processor`, tone: "ok" };
  const TIGHT = { memory: "memory tight", processor: "processor busy", disk: "disk nearly full" } as const;
  const why = strained(fit).map((r) => (fit.tone === "over" ? `not enough ${OVER_WORD[r]}` : TIGHT[r]));
  return { text: `${cost} · ${why.join(", ")}`, tone: fit.tone };
}
