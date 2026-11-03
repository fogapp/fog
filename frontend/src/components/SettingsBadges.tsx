// The counts that ride beside a Settings entry because the number is the
// reason to open it: apps overdue for a backup, and apps with something
// newer. Both read the same queries their pages do, so a badge can never
// say what the page then denies. The sidebar puts them on an area, the
// settings submenu on the page itself.

import { useQuery } from "@tanstack/react-query";
import { useApps } from "../lib/useApps";
import { backupsStatusQuery } from "../queries";
import Badge from "./Badge";

export function StaleBackupsBadge() {
  const status = useQuery({
    ...backupsStatusQuery,
    refetchInterval: 60_000,
  });
  const stale = (status.data?.apps.filter((a) => a.stale).length ?? 0) + (status.data?.files?.stale ? 1 : 0);
  return <Badge count={stale} hot label="apps overdue for a backup" />;
}

export function AppUpdatesBadge() {
  const apps = useApps({ refetchInterval: 15000 });
  const n = apps.data?.filter((a) => a.updateAvailable).length ?? 0;
  return <Badge count={n} label="apps with updates" />;
}

// The badge a page carries, if any — keyed by "area/page" so the submenu
// and the sidebar ask the same question.
export function SettingsBadge({ to }: { to: string }) {
  if (to === "storage/backups") return <StaleBackupsBadge />;
  if (to === "server/updates") return <AppUpdatesBadge />;
  return null;
}
