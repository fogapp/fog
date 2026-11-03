import { Tabs } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// Query-routed tabs (no `to`): the active id and onChange are the caller's.
// Segment-routed tabs (`to`) render react-router links and need a Router.
export function Pills() {
  return (
    <Surface>
      <Tabs
        label="Backup views"
        active="kits"
        onChange={() => {}}
        items={[
          { id: "kits", label: "Kits" },
          { id: "schedule", label: "Schedule" },
          { id: "offsite", label: "Offsite" },
        ]}
      />
    </Surface>
  );
}

export function Underline() {
  return (
    <Surface>
      <Tabs
        variant="underline"
        label="App sections"
        active="logs"
        onChange={() => {}}
        items={[
          { id: "overview", label: "Overview" },
          { id: "logs", label: "Logs" },
          { id: "settings", label: "Settings" },
          { id: "backups", label: "Backups" },
        ]}
      />
    </Surface>
  );
}

// Counts ride on the tab as badges; hot when someone should look.
export function WithCounts() {
  return (
    <Surface>
      <Tabs
        label="Inbox"
        active="all"
        onChange={() => {}}
        items={[
          { id: "all", label: "All", count: 9 },
          { id: "errors", label: "Errors", count: 2, hot: true },
          { id: "updates", label: "Updates", count: 7 },
          { id: "done", label: "Done" },
        ]}
      />
    </Surface>
  );
}

// Inline: the strip sits in a row that draws its own hairline (the app page's
// subheader), so it brings no bottom border or padding of its own.
export function Inline() {
  return (
    <Surface className="p-0">
      <div className="flex items-end justify-between gap-4 border-b border-hair px-4">
        <Tabs
          variant="underline"
          inline
          label="App sections"
          active="overview"
          onChange={() => {}}
          items={[
            { id: "overview", label: "Overview" },
            { id: "logs", label: "Logs" },
            { id: "errors", label: "Errors", count: 3, hot: true },
            { id: "settings", label: "Settings" },
          ]}
        />
        <span className="pb-2 text-hint text-ink-muted">up 4d 2h</span>
      </div>
    </Surface>
  );
}
