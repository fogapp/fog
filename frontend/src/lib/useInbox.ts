// How much is waiting for a human, in one shape, for the sidebar badge and
// the header bell — the two places that say it, and Home deliberately isn't
// one of them. One endpoint, computed over the same stores the Inbox tabs
// read, so the count and the page can never disagree. Anything that changes
// what is open invalidates inboxSummaryKey.

import { useQuery } from "@tanstack/react-query";
import { get, type InboxSummary } from "../api";

export const inboxSummaryKey = ["inbox-summary"] as const;
// Every feedback query (the Feedback tab's lists and reports) sits under
// this key, so a new report from the header refreshes whichever is open.
export const feedbackKey = ["feedback"] as const;

export interface InboxCounts {
  known: boolean;
  errors: number;
  issues: { critical: number; warning: number; info: number; total: number };
  notices: number;
  // open is every open report (the tab's number); fromPeers is the part
  // someone in another household wrote, which is what the bell counts.
  feedback: { open: number; fromPeers: number };
  fogUpdate: InboxSummary["fogUpdate"] | null;
  // total is what the badge shows: everything a human has to act on,
  // including a fog release that hasn't been ignored.
  total: number;
  hot: boolean;
  refetch: () => void;
}

export function useInboxCounts(): InboxCounts {
  const q = useQuery({
    queryKey: inboxSummaryKey,
    queryFn: () => get<InboxSummary>("/api/inbox/summary"),
    // Refetches only while the tab is visible; an idle tab asks nothing.
    refetchInterval: 30_000,
  });
  const d = q.data;
  const issues = d ? d.issues.critical + d.issues.warning + d.issues.info : 0;
  const fog = d?.fogUpdate.available && !d.fogUpdate.ignored ? 1 : 0;
  return {
    known: !!d,
    errors: d?.errors ?? 0,
    issues: { critical: d?.issues.critical ?? 0, warning: d?.issues.warning ?? 0, info: d?.issues.info ?? 0, total: issues },
    notices: d?.notices ?? 0,
    feedback: { open: d?.feedback?.open ?? 0, fromPeers: d?.feedback?.fromPeers ?? 0 },
    fogUpdate: d?.fogUpdate ?? null,
    total: (d?.errors ?? 0) + issues + (d?.notices ?? 0) + (d?.feedback?.fromPeers ?? 0) + fog,
    hot: (d?.errors ?? 0) > 0 || (d?.issues.critical ?? 0) > 0,
    refetch: () => void q.refetch(),
  };
}
