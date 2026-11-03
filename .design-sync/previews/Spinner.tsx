import { Spinner } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

export function Sizes() {
  return (
    <Surface className="flex items-center gap-6">
      <Spinner size="h-2.5 w-2.5" />
      <Spinner />
      <Spinner size="h-5 w-5" />
      <Spinner size="h-8 w-8" tone="border-fog-500" />
    </Surface>
  );
}

// Inline, in a button or a pill: size and tone follow the text around it.
export function Inline() {
  return (
    <Surface className="flex items-center gap-4">
      <span className="flex items-center gap-1.5 rounded-full bg-fog-800 px-2 py-0.5 text-xs font-medium text-ink-muted">
        <Spinner size="h-2.5 w-2.5" tone="border-fog-400" /> checking…
      </span>
      <span className="flex items-center gap-2 text-sm text-ink-muted">
        <Spinner /> Pulling image
      </span>
    </Surface>
  );
}
