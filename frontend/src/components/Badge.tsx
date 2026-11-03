// The count pill: a number that says how many of something are waiting.
// Hot means "someone should look" (open errors, a critical issue); the quiet
// tone is for counts that are just counts. Zero renders nothing — a "0"
// badge is noise, and the absence of a badge already says it.

export default function Badge({ count, hot, label }: { count: number; hot?: boolean; label?: string }) {
  if (count <= 0) return null;
  return (
    <span
      aria-label={label ? `${count} ${label}` : undefined}
      className={`inline-flex min-w-[1.25rem] items-center justify-center rounded-full border px-1.5 py-0.5 text-[10px] font-medium tabular-nums leading-none ${
        hot ? "border-warn/30 bg-warn/10 text-warn" : "border-veil-edge bg-hover text-ink-muted"
      }`}
    >
      {count > 99 ? "99+" : count}
    </span>
  );
}
