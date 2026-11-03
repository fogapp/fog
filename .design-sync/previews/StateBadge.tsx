import { StateBadge } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// Every way an app's state reads. "checking…" is its own badge: a running
// container must never sit next to a blank that later fills in.
export function States() {
  const row = (label: string, badge: React.ReactNode) => (
    <div className="flex items-center gap-3 text-sm text-ink-muted">
      <span className="w-28">{label}</span>
      {badge}
    </div>
  );
  return (
    <Surface className="space-y-2">
      {row("running", <StateBadge status={{ state: "running" }} known error={false} />)}
      {row("unhealthy", <StateBadge status={{ state: "running", healthy: false }} known error={false} />)}
      {row("stopped", <StateBadge status={{ state: "exited" }} known error={false} />)}
      {row("not known yet", <StateBadge known={false} error={false} />)}
      {row("fetch failed", <StateBadge known error />)}
    </Surface>
  );
}
