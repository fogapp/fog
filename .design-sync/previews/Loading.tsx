import { Loading } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// The label names what is being fetched — "Loading…" alone says nothing.
export function Basic() {
  return (
    <Surface className="space-y-3">
      <Loading label="Loading apps…" />
      <Loading label="Checking the NAS…" />
      <Loading label="Reading backup kits…" className="text-ink-muted" />
    </Surface>
  );
}
