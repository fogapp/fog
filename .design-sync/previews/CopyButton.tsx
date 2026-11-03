import { CopyButton } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// Always beside the thing it copies: a key, a URL, a command.
export function BesideAKey() {
  const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx1c2VyIGtleSBmb3IgZm9nIGRlcGxveXM fog-deploy@fog.lan";
  return (
    <Surface className="max-w-lg">
      <p className="mb-1 text-sm font-medium text-ink">Deploy key</p>
      <div className="flex items-center gap-2">
        <code className="min-w-0 flex-1 truncate rounded border border-fog-800 bg-code px-2 py-1.5 font-mono text-xs text-ink">{key}</code>
        <CopyButton text={key} />
      </div>
    </Surface>
  );
}

export function BesideAURL() {
  const url = "https://fog.lan/api/apps/hub/webhook/git";
  return (
    <Surface className="max-w-lg">
      <p className="mb-1 text-sm font-medium text-ink">Webhook URL</p>
      <div className="flex items-center gap-2">
        <code className="min-w-0 flex-1 truncate rounded border border-fog-800 bg-code px-2 py-1.5 font-mono text-xs text-ink">{url}</code>
        <CopyButton text={url} />
      </div>
    </Surface>
  );
}
