import type { InputHTMLAttributes, ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from "react";

// A labelled input. Values are machine strings — a hostname, a cron line, a
// certificate name — so the input is monospace, the one place Plex Mono is
// the reading face. The label is sentence case above the box; the hint turns
// amber with the field when the caller marks it aria-invalid. Every box is
// the control height (32px) on the control edge, so a field sits level
// with the buttons beside it.

interface Props extends InputHTMLAttributes<HTMLInputElement> {
  label: string;
  hint?: ReactNode;
}

// The plain control box, for an input or select laid out by hand — a row
// of a table editor, a toolbar filter — rather than through Field: the
// same height, edge and focus ring as every other control. controlArea is
// its textarea, which grows with its content instead of holding the height.
const controlBase =
  "rounded-md border border-edge bg-veil text-sm text-ink placeholder-ink-faint outline-none transition-colors hover:border-edge-hover focus:border-accent focus:ring-[3px] focus:ring-accent/15";
export const control = `h-8 px-2.5 ${controlBase}`;
export const controlArea = `px-2.5 py-2 ${controlBase}`;

// The box every Field shares. A textarea grows with its content, so it
// takes padding instead of the fixed height.
const box = (invalid: boolean, size = "h-8 px-2.5") =>
  `rounded-md border bg-veil ${size} text-[13px] text-ink placeholder-ink-faint outline-none transition-colors hover:border-edge-hover focus:outline-none focus:ring-[3px] ${
    invalid ? "border-warn focus:ring-warn/15" : "border-edge focus:border-accent focus:ring-accent/15"
  }`;

export default function Field({ label, hint, ...input }: Props) {
  const invalid = input["aria-invalid"] === true || input["aria-invalid"] === "true";
  return (
    <label className="block">
      <span className="field-label mb-1.5 block">{label}</span>
      <input {...input} className={`${box(invalid)} w-full font-mono`} />
      {hint && <span className={`mt-1.5 block text-hint ${invalid ? "text-warn" : "text-ink-muted"}`}>{hint}</span>}
    </label>
  );
}

// A choice among a few: the same box as a Field, in the reading face —
// the options are words ("SMB / CIFS", "member"), not machine strings.
// Without a label it stands inline, in a row of other controls, at the
// same height as everything else; the caller then names it with aria-label.
export function Select({ label, hint, className = "", children, ...select }: SelectHTMLAttributes<HTMLSelectElement> & {
  label?: string;
  hint?: ReactNode;
}) {
  const control = (
    <select {...select} className={`${box(false, "h-8 pl-2.5 pr-8")} font-sans text-sm ${label ? "w-full" : ""} ${className}`}>
      {children}
    </select>
  );
  if (!label) return control;
  return (
    <label className="block">
      <span className="field-label mb-1.5 block">{label}</span>
      {control}
      {hint && <span className="mt-1.5 block text-hint text-ink-muted">{hint}</span>}
    </label>
  );
}

// A Field for a value too long for one line: a public key, a certificate.
export function TextArea({ label, hint, ...area }: TextareaHTMLAttributes<HTMLTextAreaElement> & { label: string; hint?: ReactNode }) {
  return (
    <label className="block">
      <span className="field-label mb-1.5 block">{label}</span>
      <textarea {...area} className={`${box(false, "px-2.5 py-2")} w-full font-mono`} />
      {hint && <span className="mt-1.5 block text-hint text-ink-muted">{hint}</span>}
    </label>
  );
}

// The box that narrows a list as you type: no label (the list it sits over
// is its subject), named for screen readers by aria-label.
export function FilterInput({ value, onChange, label, placeholder = "Filter…" }: {
  value: string;
  onChange: (v: string) => void;
  label: string;
  placeholder?: string;
}) {
  return (
    <input
      type="search"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={placeholder}
      aria-label={label}
      className={`${box(false)} w-full max-w-xs font-sans text-sm`}
    />
  );
}
