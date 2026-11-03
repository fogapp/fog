import { Button, Field } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

export function Basic() {
  return (
    <Surface className="max-w-sm">
      <Field label="App name" placeholder="jellyfin" />
    </Surface>
  );
}

export function WithHint() {
  return (
    <Surface className="max-w-sm">
      <Field label="Hostname" defaultValue="media" hint="Reachable as media.fog.lan once the proxy picks it up." />
    </Surface>
  );
}

export function States() {
  return (
    <Surface className="max-w-sm space-y-4">
      <Field label="Git URL" defaultValue="https://gitea.lan/matthew/hub.git" />
      <Field label="Branch" defaultValue="main" disabled hint="Locked while a deploy is running." />
      <Field label="Port" type="number" defaultValue="8096" required />
    </Surface>
  );
}

// A short form the way fog's settings blocks lay one out.
export function InForm() {
  return (
    <Surface className="max-w-sm space-y-4">
      <Field label="NAS address" placeholder="192.168.1.111" />
      <Field label="Share" placeholder="media" hint="The SMB share fog mounts for apps." />
      <div className="flex justify-end gap-2">
        <Button variant="ghost">Cancel</Button>
        <Button variant="primary">Save</Button>
      </div>
    </Surface>
  );
}
