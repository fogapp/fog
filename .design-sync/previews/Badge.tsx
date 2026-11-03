import { Badge } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// Counts beside the thing they count. Hot means someone should look; zero
// renders nothing at all.
export function Counts() {
  return (
    <Surface className="flex flex-wrap items-center gap-6 text-sm text-ink-muted">
      <span className="flex items-center gap-2">Issues <Badge count={3} /></span>
      <span className="flex items-center gap-2">Errors <Badge count={12} hot /></span>
      <span className="flex items-center gap-2">Backlog <Badge count={120} /></span>
      <span className="flex items-center gap-2">Nothing waiting <Badge count={0} /> <span className="text-xs text-ink-muted">(zero renders nothing)</span></span>
    </Surface>
  );
}

// The sidebar row it usually lives in.
export function InNavRows() {
  return (
    <Surface className="w-56 space-y-0.5">
      <div className="flex items-center justify-between rounded-md bg-fog-900 px-2 py-1.5 text-sm text-ink">
        Inbox <Badge count={4} hot label="unread" />
      </div>
      <div className="flex items-center justify-between rounded-md px-2 py-1.5 text-sm text-ink-muted">
        Apps <Badge count={7} label="apps" />
      </div>
      <div className="flex items-center justify-between rounded-md px-2 py-1.5 text-sm text-ink-muted">
        Files <Badge count={0} />
      </div>
    </Surface>
  );
}
