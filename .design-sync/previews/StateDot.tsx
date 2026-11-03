import { StateDot } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// The same state as a dot, for lists.
export function InAList() {
  const apps = [
    ["Jellyfin", { state: "running" }],
    ["Hub", { state: "running", deploying: true }],
    ["Feed", { state: "running", healthy: false }],
    ["Hook", { state: "exited" }],
    ["Immich", { state: "new" }],
    ["Paperless", { state: "missing" }],
  ] as const;
  return (
    <Surface className="w-56 space-y-0.5">
      {apps.map(([name, app]) => (
        <div key={name} className="flex items-center gap-2 rounded-md px-2 py-1.5 text-sm text-ink">
          <StateDot app={app} />
          {name}
          <span className="ml-auto text-xs text-ink-muted">{app.deploying ? "deploying" : app.state}</span>
        </div>
      ))}
    </Surface>
  );
}
