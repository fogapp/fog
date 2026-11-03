import type { UpdateCheckResult } from "../api";

// Turning a check's actual result into the sentence the operator reads.
// Both helpers refuse to say "up to date" for anything that wasn't
// successfully compared: a registry that couldn't be reached and an app that
// has never been deployed are their own answers, not clean bills of health.

export type Verdict = { text: string; tone: "good" | "info" | "warn" };

export function appCheckVerdict(r: UpdateCheckResult | undefined): Verdict {
  if (!r) return { text: "The check didn't come back with a result.", tone: "warn" };
  if (r.deploying) return { text: "A deploy is running — check again when it finishes.", tone: "info" };
  if (r.error) return { text: `Couldn't check: ${r.error}`, tone: "warn" };
  if (r.updateAvailable) {
    const range = r.from && r.to ? ` (${r.from} → ${r.to})` : "";
    return { text: `Update available${range}`, tone: "info" };
  }
  if (!r.comparable) {
    return {
      text: r.source === "git"
        ? "Nothing to compare yet — this app hasn't been deployed from the repository."
        : "Nothing to compare yet — this app hasn't been deployed from its image.",
      tone: "warn",
    };
  }
  return { text: "Up to date ✓", tone: "good" };
}

export const toneClass: Record<Verdict["tone"], string> = {
  good: "text-accent",
  info: "text-accent",
  warn: "text-warn",
};
