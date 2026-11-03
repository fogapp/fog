// The one bell. Its badge is everything waiting for a human — errors,
// issues, notices, feedback a peer sent, and a fog release that hasn't been
// ignored — and its
// panel is the short way into each: a row per kind linking to its Inbox
// tab (notices count under Issues, where they are listed), and the fog
// update with its two verbs right there. Two bells (notices, fog updates) used to share this
// corner; one attention channel is the point.

import { useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { post } from "../api";
import { errorText } from "./Async";
import Badge from "./Badge";
import SettingsLink from "./SettingsLink";
import { useDismissable } from "../lib/useDismissable";
import { paths } from "../lib/paths";
import { versionLabel } from "../lib/format";
import { inboxSummaryKey, useInboxCounts } from "../lib/useInbox";
import { usePollUntilRestart } from "../lib/usePollUntilRestart";

export default function InboxBell() {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const counts = useInboxCounts();
  useDismissable(ref, open, () => setOpen(false));

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen(!open)}
        aria-label={counts.total > 0 ? `Inbox: ${counts.total} waiting` : "Inbox"}
        aria-haspopup="dialog"
        aria-expanded={open}
        className={`flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium transition-colors ${
          counts.total === 0
            ? "text-ink-muted hover:bg-fog-800 hover:text-ink"
            : counts.hot
              ? "bg-warn/10 text-warn hover:bg-warn/10"
              : "bg-fog-800 text-ink hover:bg-fog-700"
        }`}
      >
        <BellIcon />
        {counts.total > 0 && <span className="tabular-nums">{counts.total > 99 ? "99+" : counts.total}</span>}
      </button>

      {open && (
        <div
          role="dialog"
          aria-label="Inbox summary"
          className="absolute right-0 top-9 z-30 w-96 overflow-hidden rounded-xl border border-veil-edge bg-fog-900 shadow-xl shadow-black/40"
        >
          <ul className="divide-y divide-divider">
            <Row
              to={paths.inbox("errors")}
              label="Errors"
              count={counts.errors}
              hot={counts.errors > 0}
              hint={counts.errors > 0 ? "open and regressed, across every app" : "nothing broken"}
              onGo={() => setOpen(false)}
            />
            <Row
              to={paths.inbox("issues")}
              label="Issues"
              count={counts.issues.total + counts.notices}
              hot={counts.issues.critical > 0}
              hint={
                counts.issues.total + counts.notices > 0
                  ? [
                      counts.notices && `${counts.notices} ${counts.notices === 1 ? "notice" : "notices"}`,
                      counts.issues.critical && `${counts.issues.critical} critical`,
                      counts.issues.warning && `${counts.issues.warning} warning`,
                      counts.issues.info && `${counts.issues.info} info`,
                    ].filter(Boolean).join(" · ")
                  : "configuration checks out"
              }
              onGo={() => setOpen(false)}
            />
            <Row
              to={paths.inbox("feedback")}
              label="Feedback"
              count={counts.feedback.fromPeers}
              hint={
                counts.feedback.open > 0
                  ? [
                      `${counts.feedback.open} open`,
                      counts.feedback.fromPeers > 0 && `${counts.feedback.fromPeers} from peers`,
                    ].filter(Boolean).join(" · ")
                  : "nothing reported"
              }
              onGo={() => setOpen(false)}
            />
          </ul>
          {counts.fogUpdate?.available && !counts.fogUpdate.ignored && (
            <FogUpdateRow update={counts.fogUpdate} onClose={() => setOpen(false)} />
          )}
          <Link
            to={paths.inbox()}
            onClick={() => setOpen(false)}
            className="block border-t border-hair px-4 py-2 text-center text-xs text-ink-muted hover:bg-fog-800 hover:text-ink"
          >
            Open the Inbox →
          </Link>
        </div>
      )}
    </div>
  );
}

function Row({ to, label, count, hot, hint, onGo }: {
  to: string; label: string; count: number; hot?: boolean; hint: string; onGo: () => void;
}) {
  return (
    <li>
      <Link to={to} onClick={onGo} className="flex items-center gap-3 px-4 py-2.5 hover:bg-fog-800">
        <span className="w-16 shrink-0 text-sm font-medium text-ink">{label}</span>
        <span className="min-w-0 flex-1 truncate text-xs text-ink-muted">{hint}</span>
        {count > 0 ? <Badge count={count} hot={hot} /> : <span className="text-xs text-accent">✓</span>}
      </Link>
    </li>
  );
}

// The fog release, with the quick path — Update now — and Ignore. The
// page reloads only once /api/setup/status answers with a different
// version: proof the new process is up. Past the deadline the row says so
// instead of reloading into whatever is running. The release channel and a
// manual check live on Settings › Server › Updates.
function FogUpdateRow({ update, onClose }: {
  update: NonNullable<ReturnType<typeof useInboxCounts>["fogUpdate"]>;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const restart = usePollUntilRestart();

  async function apply() {
    const oldVersion = update.currentVersion;
    setError("");
    try {
      await post("/api/system/self-update");
    } catch (e) {
      // The row stays and says what happened — closing here would read as
      // "the update is underway".
      setError(errorText(e, "The update couldn't be started"));
      return;
    }
    restart.begin({ isUp: (s) => !!s.version && s.version !== oldVersion });
  }

  const ignore = useMutation({
    mutationFn: () => post("/api/system/self-update/ignore", { version: update.latestVersion }),
    onSuccess: () => qc.invalidateQueries({ queryKey: inboxSummaryKey }),
    onError: (e) => setError(errorText(e, "Couldn't ignore this version")),
  });

  return (
    <div className="border-t border-hair bg-accent/10 px-4 py-3">
      {restart.waiting ? (
        <div className="space-y-1">
          <p className="flex items-center gap-2 text-sm text-ink">
            <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-fog-300 border-t-transparent" />
            Updating — fogd is restarting…
          </p>
          <p className="text-xs text-ink-muted">This page reloads automatically on the new version.</p>
        </div>
      ) : (
        <>
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0">
              <p className="text-sm font-semibold text-ink">Fog {versionLabel(update.latestVersion ?? "")} is available</p>
              <p className="text-xs text-ink-muted">You're running {versionLabel(update.currentVersion ?? "")}.</p>
            </div>
            <div className="flex shrink-0 gap-2">
              <button onClick={apply}
                className="rounded-md bg-accent px-3 py-1 text-xs font-semibold text-accent-on hover:bg-accent-hover">
                Update now
              </button>
              <button onClick={() => ignore.mutate()} disabled={ignore.isPending} title="Hide until the next version"
                className="rounded-md border border-edge px-2.5 py-1 text-xs text-ink-muted hover:bg-fog-800 disabled:opacity-50">
                Ignore
              </button>
            </div>
          </div>
          {update.notes && (
            <details className="mt-2">
              <summary className="cursor-pointer text-xs text-ink-muted hover:text-ink">release notes</summary>
              <pre className="mt-1 max-h-40 overflow-y-auto whitespace-pre-wrap rounded-md border border-hair bg-code p-2 text-xs text-ink-muted">
                {update.notes}
              </pre>
            </details>
          )}
          {restart.timedOut && (
            <p className="mt-2 text-xs text-warn">
              The server hasn't come back on {versionLabel(update.latestVersion ?? "")}. The update may still be
              finishing — or it failed; check{" "}
              <SettingsLink to="server/updates" anchor="fog" onClick={onClose} />.
            </p>
          )}
          {error && <p className="mt-2 text-xs text-warn">{error}</p>}
        </>
      )}
    </div>
  );
}

function BellIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden className="h-4 w-4">
      <path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9" />
      <path d="M10.3 21a1.94 1.94 0 0 0 3.4 0" />
    </svg>
  );
}
