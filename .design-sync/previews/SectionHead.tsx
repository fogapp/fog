import { Button, SectionHead } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// A section head on its own: the title, then whatever ends the line.
export function Basic() {
  return (
    <Surface>
      <SectionHead title="Peers" />
      <p className="mt-3 text-sm text-ink-muted">studio-mini · attic-pi · cabin-01</p>
    </Surface>
  );
}

// With a right-hand label after the hairline.
export function WithCount() {
  return (
    <Surface>
      <SectionHead title="Apps" right={<span className="text-hint text-ink-muted">6 running</span>} />
      <p className="mt-3 text-sm text-ink-muted">jellyfin · immich · paperless · vaultwarden · syncthing</p>
    </Surface>
  );
}

// With the group's action.
export function WithAction() {
  return (
    <Surface>
      <SectionHead title="Disks" right={<Button size="sm">Add disk</Button>} />
      <p className="mt-3 text-sm text-ink-muted">4 of 6 bays</p>
    </Surface>
  );
}
