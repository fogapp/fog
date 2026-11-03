// Notices: things fog needs a human for that it cannot re-derive on the
// next sweep — revoke a deploy key on the git host, a backup that stopped
// landing. They stay until explicitly dismissed, unlike issues, which
// resolve themselves when the world changes. They used to have a tab of
// their own; they now lead the Issues tab, so the Inbox has one list of
// "things about this box that need you" rather than two.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, type Notice } from "../../api";
import Button from "../../components/Button";
import { timeAgo } from "../../lib/format";
import { inboxSummaryKey } from "../../lib/useInbox";
import { DetailPane, Tag } from "./Split";
import { displayName } from "../../lib/displayName";

const noticesKey = ["notices"] as const;

// A notice's selection in the Issues tab's ?sel=: prefixed so it can never
// collide with an issue's id.
export const noticeSel = (id: number) => `n${id}`;

export function useNotices(enabled = true) {
  return useQuery({
    queryKey: noticesKey,
    queryFn: () => get<{ notices: Notice[] }>("/api/notices"),
    refetchInterval: 60_000,
    enabled,
  });
}

// Dismiss is awaited through the refetch: the row leaves as part of the
// click completing, and the badge follows in the same beat.
export function useDismissNotice(onDone: () => void, onError: (e: unknown) => void) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => post(`/api/notices/${id}/dismiss`),
    onSuccess: async () => {
      await Promise.all([
        qc.invalidateQueries({ queryKey: noticesKey }),
        qc.invalidateQueries({ queryKey: inboxSummaryKey }),
      ]);
      onDone();
    },
    onError,
  });
}

export function NoticeBadge() {
  return <span className="rounded-full border border-hair px-2 py-px text-[11px] font-medium text-ink-muted">Notice</span>;
}

export function NoticeDetail({ notice, dismiss, onBack }: {
  notice: Notice;
  dismiss: ReturnType<typeof useDismissNotice>;
  onBack: () => void;
}) {
  return (
    <DetailPane onBack={onBack}>
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <NoticeBadge />
            {notice.app && <Tag>{notice.app}</Tag>}
          </div>
          <h2 className="mt-2 break-words text-lg font-semibold text-ink">{displayName(notice.title)}</h2>
          <p className="mt-1 text-xs text-ink-muted">
            {timeAgo(notice.createdAt)} · stays until you dismiss it
          </p>
        </div>
        <Button size="sm" onClick={() => dismiss.mutate(notice.id)} disabled={dismiss.isPending}>
          {dismiss.isPending && dismiss.variables === notice.id ? "…" : "Dismiss"}
        </Button>
      </div>
      {notice.body && (
        <pre className="mt-4 whitespace-pre-wrap break-all rounded-md border border-hair bg-code p-3 text-xs text-ink-muted">
          {displayName(notice.body, { keepNewlines: true })}
        </pre>
      )}
    </DetailPane>
  );
}
