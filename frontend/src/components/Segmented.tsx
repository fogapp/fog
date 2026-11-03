// One of a few, side by side: Light · Dark · System, Basic · Advanced. A
// radio group drawn as a 32px track holding 24px options — the control
// height, like every button and field — the chosen one lifted onto the
// surface in the beacon.
// value undefined means the stored choice hasn't arrived yet, so nothing is
// lit and nothing can be chosen — a highlight is a claim about the server.

export default function Segmented<T extends string>({ label, options, value, onChange, disabled }: {
  // The group's accessible name.
  label: string;
  options: readonly { id: T; label: string; hint?: string }[];
  value: T | undefined;
  onChange: (id: T) => void;
  disabled?: boolean;
}) {
  return (
    <div className="inline-flex h-8 items-center gap-0.5 rounded-md border border-edge bg-hover p-[3px]" role="radiogroup" aria-label={label}>
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          role="radio"
          aria-checked={value === o.id}
          onClick={() => onChange(o.id)}
          disabled={disabled || value === undefined}
          title={o.hint}
          className={`h-6 rounded px-3 text-cell font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
            value === o.id ? "bg-veil text-accent ring-1 ring-edge" : "text-ink-muted hover:text-ink"
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
