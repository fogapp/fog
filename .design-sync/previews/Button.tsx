import { Button } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// The five variants, as they appear in fog: secondary is the default and the
// one most rows carry; primary is the single committing action on a page.
export function Variants() {
  return (
    <Surface className="flex flex-wrap items-center gap-2">
      <Button variant="primary">Deploy</Button>
      <Button>Restart</Button>
      <Button variant="info">Update to v1.14</Button>
      <Button variant="danger">Remove app</Button>
      <Button variant="ghost">Cancel</Button>
    </Surface>
  );
}

export function Sizes() {
  return (
    <Surface className="flex flex-wrap items-center gap-2">
      <Button size="sm">Small</Button>
      <Button size="md">Medium (default)</Button>
      <Button variant="primary" size="sm">Save</Button>
      <Button variant="primary">Save changes</Button>
    </Surface>
  );
}

export function Disabled() {
  return (
    <Surface className="flex flex-wrap items-center gap-2">
      <Button variant="primary" disabled>Deploying…</Button>
      <Button disabled>Restart</Button>
      <Button variant="danger" disabled>Remove app</Button>
    </Surface>
  );
}

// Actions the way a page header groups them: primary on the right, the rest
// beside it, all one size.
export function ActionRow() {
  return (
    <Surface>
      <div className="flex items-center justify-end gap-2 rounded-lg border border-fog-800 bg-fog-900 px-4 py-3">
        <Button variant="ghost">Discard</Button>
        <Button>Preview</Button>
        <Button variant="primary">Publish</Button>
      </div>
    </Surface>
  );
}
