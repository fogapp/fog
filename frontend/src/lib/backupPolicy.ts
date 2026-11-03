// Whether an update or a rollback starts with a snapshot — the frontend
// twin of backup.SnapshotsBeforeUpdate. Never for an app whose backups are
// off; otherwise the app's own beforeUpdate, falling back to the fleet
// default in Settings › Storage › Backups (backup_before_update, empty = on).
// Settings not loaded yet (or not readable) reads as the default: on.

import { useQuery } from "@tanstack/react-query";
import type { AppSpec, Settings } from "../api";
import { settingsQuery } from "../queries";

export function fleetSnapshotsBeforeUpdate(settings?: Pick<Settings, "backup_before_update">): boolean {
  return settings?.backup_before_update !== "off";
}

function snapshotsBeforeUpdate(spec: AppSpec, settings?: Pick<Settings, "backup_before_update">): boolean {
  if (!spec.backup?.enabled) return false;
  return spec.backup.beforeUpdate ?? fleetSnapshotsBeforeUpdate(settings);
}

// snapshotsBeforeUpdate against the live settings, for components.
export function useSnapshotsBeforeUpdate(spec: AppSpec): boolean {
  const settings = useQuery(settingsQuery);
  return snapshotsBeforeUpdate(spec, settings.data);
}
