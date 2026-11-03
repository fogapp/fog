// Shared loading / empty / failure messaging.
//
// The rule across Fog's UI: never state a fact before the data behind it has
// arrived. "No apps yet", "up to date", "not installed" and "not mounted" are
// all claims — they wait for an answer. Until one arrives the UI says what it
// is actually doing, and when the answer never comes it says that instead of
// falling back to the empty case, which reads as a confident "nothing here".

import Button from "./Button";

export function Spinner({ size = "h-3 w-3", tone = "border-accent" }: { size?: string; tone?: string }) {
  return <span className={`inline-block shrink-0 animate-spin rounded-full border-2 border-t-transparent ${size} ${tone}`} />;
}

// Loading: the neutral "working on it" line. Give it a label that names what
// is being fetched — "Loading…" alone tells the operator nothing.
export function Loading({ label, className = "" }: { label: string; className?: string }) {
  return (
    <p className={`flex items-center gap-2 text-sm text-ink-muted ${className}`}>
      <Spinner />
      {label}
    </p>
  );
}

// LoadError: a fetch that never landed. Distinct from an empty result, and
// always offers the retry, because the usual cause is a blip. It needs you,
// so it is amber — never red; nothing here is an emergency.
export function LoadError({ label, error, onRetry }: { label: string; error?: unknown; onRetry?: () => void }) {
  const detail = errorText(error, "");
  return (
    <div className="space-y-2 rounded-lg border border-veil-edge bg-veil p-4">
      <p className="flex items-center gap-2 text-sm text-warn">
        <span aria-hidden className="h-1.5 w-1.5 shrink-0 rounded-full bg-warn" />
        {label}
      </p>
      {detail && <p className="break-words font-mono text-xs text-ink-muted">{detail}</p>}
      {onRetry && (
        <Button size="sm" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}

// StaleNote: shown when a polling query starts failing but stale data is
// still on screen. The numbers are real, they're just not current — saying so
// beats letting them quietly rot.
export function StaleNote({ what }: { what: string }) {
  return (
    <p className="mt-2 text-hint text-ink-muted">
      Can't reach fogd — {what} may be out of date.
    </p>
  );
}

export function errorText(e: unknown, fallback: string): string {
  if (e instanceof Error && e.message) return e.message;
  if (typeof e === "string" && e) return e;
  return fallback;
}

// Query state boiled down to the four cases every list has to distinguish.
// Errors only win when there is nothing to show: a failed background refetch
// on a list that already has data is staleness, not failure.
export type Phase = "loading" | "error" | "empty" | "ready";

// Pass the query and the length of whatever it resolves to; pass undefined
// for the count when the data hasn't arrived.
export function phaseOf(q: { isError: boolean }, count: number | undefined): Phase {
  if (count === undefined) return q.isError ? "error" : "loading";
  return count === 0 ? "empty" : "ready";
}
