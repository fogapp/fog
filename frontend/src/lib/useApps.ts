// The one query for GET /api/apps. Every view of the app list reads through
// here, so the two shapes — the plain list and the stats-enriched one — live
// under a single ["apps"] prefix with one canonical key each. A mutation that
// changes app state invalidates the prefix, never a leaf: three pages holding
// three private keys is how the sidebar keeps a green dot on an app the
// dashboard just stopped.

import { useQuery } from "@tanstack/react-query";
import { get, type AppStatus } from "../api";

// The prefix every apps query lives under. Mutations that change app state
// invalidate this so the list and the stats view can never disagree.
export const appsKey = ["apps"] as const;

const appsListKey = [...appsKey, "list"] as const;
const appsStatsKey = [...appsKey, "stats"] as const;

export function useApps(opts?: {
  // stats=1 asks fogd to attach CPU/memory figures; the list shape answers
  // faster and is enough for anything that only names apps.
  stats?: boolean;
  refetchInterval?: number;
  enabled?: boolean;
  staleTime?: number;
}) {
  const stats = opts?.stats ?? false;
  return useQuery({
    queryKey: stats ? appsStatsKey : appsListKey,
    queryFn: () => get<AppStatus[]>(stats ? "/api/apps?stats=1" : "/api/apps"),
    refetchInterval: opts?.refetchInterval,
    enabled: opts?.enabled,
    staleTime: opts?.staleTime,
  });
}
