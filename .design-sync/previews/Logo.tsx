import { Logo } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// The beacon at the sizes it appears: the 20px wordmark mark, and larger
// where it stands alone (login, the wizard).
export function Sizes() {
  return (
    <Surface className="flex items-end gap-6">
      <Logo />
      <Logo className="h-6 w-6" />
      <Logo className="h-9 w-9" />
    </Surface>
  );
}

// The wordmark: the mark + "fog" in Plus Jakarta Sans 600.
export function Wordmark() {
  return (
    <Surface className="flex items-center gap-2.5 text-ink">
      <Logo />
      <span className="text-[17px] font-semibold">fog</span>
      <span className="text-xs text-ink-muted">v0.12.20</span>
    </Surface>
  );
}
