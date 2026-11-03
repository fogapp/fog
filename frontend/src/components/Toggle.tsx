// The switch: a 34×19 pill that reads as on/off at a glance where a
// checkbox would read as a form. The knob is the beacon when on; the track
// is the beacon at 30%, never a full fill — the switch is a state, not a
// button. Label is what the switch controls, said in the sentence case the
// rest of the copy uses.

import type { InputHTMLAttributes } from "react";

interface Props extends Omit<InputHTMLAttributes<HTMLInputElement>, "type" | "onChange"> {
  checked: boolean;
  onChange: (on: boolean) => void;
  label?: string;
}

export default function Toggle({ checked, onChange, label, className = "", disabled, ...rest }: Props) {
  return (
    <label
      className={`inline-flex items-center gap-2 text-hint text-ink-muted ${disabled ? "cursor-not-allowed opacity-50" : "cursor-pointer"} ${className}`}
    >
      <span className="relative inline-block h-[19px] w-[34px] shrink-0">
        <input
          {...rest}
          type="checkbox"
          role="switch"
          aria-checked={checked}
          checked={checked}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
          className="peer absolute inset-0 m-0 h-full w-full cursor-[inherit] appearance-none rounded-full border border-veil-edge bg-fog-700 transition-colors checked:bg-accent/30"
        />
        <span
          aria-hidden
          className="pointer-events-none absolute left-[2px] top-[2px] h-[13px] w-[13px] rounded-full bg-fog-400 transition-[left,background-color] duration-150 peer-checked:left-[17px] peer-checked:bg-accent"
        />
      </span>
      {label && <span>{label}</span>}
    </label>
  );
}
