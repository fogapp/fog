import { Button, PageHeader, StateBadge, Tabs } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

export function Basic() {
  return (
    <Surface>
      <PageHeader title="Backups" description="Nightly kits for every app, kept on the NAS and mirrored offsite." />
    </Surface>
  );
}

// The app page: the state pill rides beside the title, page-wide actions on
// the right.
export function WithBadgeAndActions() {
  return (
    <Surface>
      <PageHeader
        title="Jellyfin"
        description="Media server · jellyfin/jellyfin:10.10.3"
        badge={<StateBadge status={{ state: "running" }} known error={false} />}
        actions={
          <>
            <Button>Restart</Button>
            <Button variant="info">Update to 10.10.5</Button>
            <Button variant="primary">Open</Button>
          </>
        }
      />
    </Surface>
  );
}

// The tab strip belongs under the header, as its children.
export function WithTabs() {
  return (
    <Surface>
      <PageHeader title="Settings" description="Everything this server runs on.">
        <Tabs
          variant="underline"
          label="Settings areas"
          active="storage"
          onChange={() => {}}
          items={[
            { id: "overview", label: "Overview" },
            { id: "storage", label: "Storage" },
            { id: "network", label: "Network" },
            { id: "updates", label: "Updates", count: 2, hot: true },
          ]}
        />
      </PageHeader>
    </Surface>
  );
}
