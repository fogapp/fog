// The reads many pages share, each defined once: its key, its URL and the
// type it answers with. A page spreads one into useQuery and adds its own
// polling; a mutation invalidates by the same queryKey, so every copy on
// screen refreshes together.

import { queryOptions } from "@tanstack/react-query";
import {
  get,
  type AppBackupStatus,
  type BackupStoreSummary,
  type CatalogResponse,
  type ComputeOverview,
  type DisksResponse,
  type FederationInfo,
  type FileFedShare,
  type FileFedSubscription,
  type FilesBackupStatus,
  type MCPToken,
  type Me,
  type NasResponse,
  type PersonMe,
  type ReachResponse,
  type Settings,
  type SetupStatus,
  type SystemStatus,
  type TunnelStatus,
  type VpnStatus,
} from "./api";
import { peerSharesKey } from "./lib/filesFed";

export const settingsQuery = queryOptions({
  queryKey: ["settings"],
  queryFn: () => get<Settings>("/api/settings"),
});

export const systemQuery = queryOptions({
  queryKey: ["system"],
  queryFn: () => get<SystemStatus>("/api/system"),
});

export const catalogQuery = queryOptions({
  queryKey: ["catalog"],
  queryFn: () => get<CatalogResponse>("/api/catalog"),
});

export const nasQuery = queryOptions({
  queryKey: ["nas"],
  queryFn: () => get<NasResponse>("/api/nas"),
});

// The federation document. Its key prefixes the ledger, mirror and
// peer-catalog reads, so invalidating it refreshes those too.
export const federationQuery = queryOptions({
  queryKey: ["federation"],
  queryFn: () => get<FederationInfo>("/api/federation"),
});

// Every outbound file share, to any peer.
export const peerSharesQuery = queryOptions({
  queryKey: peerSharesKey(),
  queryFn: () => get<{ shares: FileFedShare[] }>("/api/files/peer-shares"),
});

// Every inbound file subscription.
export const subscriptionsQuery = queryOptions({
  queryKey: ["files", "subscriptions"],
  queryFn: () => get<{ subscriptions: FileFedSubscription[] }>("/api/files/subscriptions"),
});

// Whether the first-run wizard still has to run.
export const setupStatusQuery = queryOptions({
  queryKey: ["setup-status"],
  queryFn: () => get<SetupStatus>("/api/setup/status"),
});

// Who is signed in, and for a member, the person view the member shell needs.
export const meQuery = queryOptions({
  queryKey: ["me"],
  queryFn: () => get<Me>("/api/auth/me"),
});

export const personMeQuery = queryOptions({
  queryKey: ["person-me"],
  queryFn: () => get<PersonMe>("/api/people/me"),
});

// Away access and the public tunnel: their own pages, and the dependency list.
export const vpnQuery = queryOptions({
  queryKey: ["vpn"],
  queryFn: () => get<VpnStatus>("/api/vpn"),
});

export const tunnelQuery = queryOptions({
  queryKey: ["tunnel"],
  queryFn: () => get<TunnelStatus>("/api/tunnel"),
});

// Settings › Compute: paired machines and the pools apps see. Machines come
// and go online on their own, so it's polled while the page is open.
export const computeQuery = queryOptions({
  queryKey: ["compute"],
  queryFn: () => get<ComputeOverview>("/api/compute"),
  refetchInterval: 5000,
});

export const mcpTokensQuery = queryOptions({
  queryKey: ["mcp-tokens"],
  queryFn: () => get<{ tokens: MCPToken[] }>("/api/settings/mcp-tokens"),
});

export const reachQuery = queryOptions({
  queryKey: ["domain-reach"],
  queryFn: () => get<ReachResponse>("/api/domain/reach"),
});

// The domain page's status read (typed where it is read, in Domain.tsx);
// the reach section refreshes it after a change.
export const domainStatusKey = ["domain-status"] as const;

export const disksQuery = queryOptions({
  queryKey: ["disks"],
  queryFn: () => get<DisksResponse>("/api/system/disks"),
});

// Error groups: per app, fleet-wide, and one group's detail. A triage
// action anywhere refreshes all three.
export const appErrorsKey = ["app-errors"] as const;
export const fleetErrorsKey = ["fleet-errors"] as const;
export const errorGroupKey = ["error-group"] as const;

// The fleet's backup freshness: one row per app, plus Drive's snapshots.
export const backupsStatusQuery = queryOptions({
  queryKey: ["backups-status"],
  queryFn: () => get<{ apps: AppBackupStatus[]; files: FilesBackupStatus; store?: BackupStoreSummary }>("/api/backups/status"),
});
