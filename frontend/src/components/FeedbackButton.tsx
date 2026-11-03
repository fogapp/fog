// The bug button: the short way to say "that isn't what I expected" or "I
// wish this worked differently" at the moment it happens, without leaving
// the page. The person writes in their own words; fog attaches what it knew
// — this page, what the browser saw lately, fogd's recent logs, what the
// Inbox holds — so the report still means something when someone reads it
// later, here or on the peer it goes to.
//
// The browser's part is photographed when the button is pressed, not when
// Send is: the dialog itself shouldn't become the last page visited.

import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { Link, useLocation } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { post, type FeedbackAttached, type FeedbackItem } from "../api";
import { errorText } from "./Async";
import Button from "./Button";
import Modal from "./Modal";
import { crumbsFor } from "../lib/breadcrumb";
import { recordNavigation, snapshot, type ClientSnapshot } from "../lib/clientTrail";
import { peerLabel, plural } from "../lib/format";
import { appNameFromPath, paths } from "../lib/paths";
import { feedbackKey, inboxSummaryKey } from "../lib/useInbox";
import { federationQuery } from "../queries";
import { controlArea } from "./Field";

interface Opened {
  snap: ClientSnapshot;
  page: { path: string; search: string; title: string };
}

export default function FeedbackButton() {
  const location = useLocation();
  const [opened, setOpened] = useState<Opened | null>(null);
  // The draft outlives the dialog: a stray Escape or backdrop click must
  // not throw away what someone was halfway through writing.
  const [draft, setDraft] = useState("");

  useEffect(() => recordNavigation(location.pathname + location.search), [location.pathname, location.search]);

  return (
    <>
      <button
        type="button"
        onClick={() =>
          setOpened({
            snap: snapshot(),
            page: { path: location.pathname, search: location.search, title: document.title },
          })
        }
        aria-label="Send feedback"
        aria-haspopup="dialog"
        title="Something unexpected? Send feedback"
        className="flex items-center rounded-full px-2.5 py-1 text-ink-muted transition-colors hover:bg-fog-800 hover:text-ink"
      >
        <BugIcon />
      </button>
      {/* Portalled out of the header: its backdrop-blur makes it the
          containing block for fixed children, which would pin the dialog
          inside a 56px strip instead of over the page. */}
      {opened &&
        createPortal(
          <FeedbackDialog opened={opened} draft={draft} setDraft={setDraft} onClose={() => setOpened(null)} />,
          document.body,
        )}
    </>
  );
}

type Created = { feedback: FeedbackItem; attached: FeedbackAttached };

function FeedbackDialog({ opened, draft, setDraft, onClose }: {
  opened: Opened;
  draft: string;
  setDraft: (v: string) => void;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const submit = useMutation({
    mutationFn: () => post<Created>("/api/feedback", { body: draft, page: opened.page, client: opened.snap }),
    onSuccess: () => {
      setDraft("");
      return Promise.all([
        qc.invalidateQueries({ queryKey: inboxSummaryKey }),
        qc.invalidateQueries({ queryKey: feedbackKey }),
      ]);
    },
  });

  // "Filed", never "sent": whether a peer has it is the delivery line's claim
  // to make, on the peer's own answer.
  return (
    <Modal title={submit.data ? "Feedback filed" : "Send feedback"} onClose={onClose} width="max-w-xl">
      {submit.data ? (
        <Sent result={submit.data} onClose={onClose} />
      ) : (
        <form
          className="space-y-4 text-sm"
          onSubmit={(e) => {
            e.preventDefault();
            if (draft.trim() && !submit.isPending) submit.mutate();
          }}
          // On the form, not the textarea: ⌘↵ should still send after a click
          // on the attachments list has moved focus off the text.
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              e.currentTarget.requestSubmit();
            }
          }}
        >
          <p className="text-ink-muted">
            Something didn't do what you expected, or you wish it worked differently? Say it in your own words.
          </p>
          <textarea
            autoFocus
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            rows={6}
            maxLength={10_000}
            placeholder="What happened, and what did you expect instead?"
            aria-label="Your feedback"
            className={`${controlArea} block w-full resize-y leading-relaxed`}
          />
          <Attachments opened={opened} />
          <Destinations />
          {submit.isError && <p className="text-hint text-warn">{errorText(submit.error, "Couldn't send the feedback")}</p>}
          <div className="flex items-center justify-end gap-2 border-t border-hair pt-4">
            <span className="mr-auto text-hint text-ink-muted">⌘↵ sends</span>
            <Button type="button" variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
            <Button type="submit" variant="primary" size="sm" disabled={!draft.trim() || submit.isPending}>
              {submit.isPending ? "Sending…" : "Send"}
            </Button>
          </div>
        </form>
      )}
    </Modal>
  );
}

// What rides along, said before Send is pressed. The browser figures are
// the real ones from the snapshot; the server's are named as kinds, since
// fogd reads them at the moment of sending.
function Attachments({ opened }: { opened: Opened }) {
  const { snap, page } = opened;
  const failed = snap.requests.filter((r) => /→ (failed|[45]\d\d)/.test(r.message)).length;
  const crumb = crumbsFor(page.path, page.search, { basic: false }).map((c) => c.label).join(" / ");
  const app = appNameFromPath(page.path);
  const browser = [
    snap.errors.length > 0 && plural(snap.errors.length, "error"),
    failed > 0 && plural(failed, "failed request"),
    snap.navigation.length > 1 && `the last ${plural(snap.navigation.length, "page")} visited`,
  ].filter(Boolean);

  return (
    <details className="rounded-md border border-hair px-3 py-2">
      <summary className="cursor-pointer text-hint text-ink-muted hover:text-ink">
        Attached so it can be followed up — this page, what the browser saw, and fog's recent state
      </summary>
      <ul className="mt-2 space-y-1 text-hint text-ink-muted">
        <li><span className="text-ink">This page</span> — {crumb || page.path}</li>
        <li>
          <span className="text-ink">This browser</span> — {browser.length > 0 ? browser.join(", ") : "no errors or failed requests since it loaded"}; its version, window size and time zone
        </li>
        {app && <li><span className="text-ink">{app}</span> — its containers, recent logs and deploys</li>}
        <li><span className="text-ink">fogd</span> — its recent logs and warnings, open errors, issues and notices, recent activity, host load</li>
      </ul>
    </details>
  );
}

// Where the report goes. Nothing is said about peers until the federation
// list has actually loaded — "stays here" before then could be untrue.
function Destinations() {
  const fed = useQuery({
    ...federationQuery,
    staleTime: 30_000,
  });
  if (!fed.data) return null;
  const sends = fed.data.peers.flatMap((p) => {
    const cap = p.capabilities.find((c) => c.capability === "feedback.ingest" && c.direction === "use");
    const mode = String(cap?.config?.mode ?? "");
    return cap && (mode === "note" || mode === "full") ? [{ id: p.id, name: peerLabel(p), mode }] : [];
  });
  return (
    <p className="text-hint text-ink-muted">
      Filed in Inbox › Feedback
      {sends.length === 0
        ? "."
        : sends.map((s, i) => (
            <span key={s.id}>
              {i === 0 ? ", and sent to " : i === sends.length - 1 ? " and " : ", "}
              <span className="text-ink-muted">{s.name}</span> ({s.mode === "full" ? "with everything attached" : "just the words and the page"})
            </span>
          ))}
      {sends.length > 0 && "."}
    </p>
  );
}

// After Send: what is now true, each part on its own evidence.
function Sent({ result, onClose }: { result: Created; onClose: () => void }) {
  const { feedback: f, attached: a } = result;
  const parts = [
    `${plural(a.logs, "fogd log line")}${a.warnings ? ` (${plural(a.warnings, "warning")})` : ""}`,
    a.errors > 0 && plural(a.errors, "open error"),
    a.issues > 0 && plural(a.issues, "open issue"),
    a.browser && "what the browser saw",
    a.app && `${a.app}'s containers and logs`,
  ].filter(Boolean);
  return (
    <div className="space-y-4 text-sm">
      <p className="text-ink">
        Thanks — it's filed in{" "}
        <Link to={paths.inbox("feedback", { sel: String(f.id) })} onClick={onClose} className="text-accent hover:underline">
          Inbox › Feedback
        </Link>
        .
      </p>
      {(f.deliveries ?? []).length > 0 && (
        <ul className="space-y-1.5">
          {f.deliveries!.map((d) => (
            <li key={d.peerId} className="text-hint">
              {d.state === "delivered" ? (
                <span className="text-ink-muted"><span className="text-accent">✓</span> Delivered to {d.peer}</span>
              ) : d.state === "refused" ? (
                <span className="text-warn">{d.peer} refused it: {d.lastError}</span>
              ) : (
                <span className="text-ink-muted">
                  Not delivered to {d.peer} yet — fog keeps trying in the background.
                  {d.lastError && <span className="block break-words text-ink-muted">Last try: {d.lastError}</span>}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
      <p className="text-hint text-ink-muted">Attached: {parts.join(", ")}.</p>
      {a.gaps && a.gaps.length > 0 && (
        <div className="text-hint text-warn">
          Couldn't attach:
          <ul className="mt-1 list-disc pl-5">
            {a.gaps.map((g) => <li key={g} className="break-words">{g}</li>)}
          </ul>
        </div>
      )}
      <div className="flex justify-end border-t border-hair pt-4">
        <Button size="sm" onClick={onClose}>Close</Button>
      </div>
    </div>
  );
}

function BugIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden className="h-4 w-4">
      <path d="m8 2 1.88 1.88" />
      <path d="M14.12 3.88 16 2" />
      <path d="M9 7.13v-1a3.003 3.003 0 1 1 6 0v1" />
      <path d="M12 20c-3.3 0-6-2.7-6-6v-3a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v3c0 3.3-2.7 6-6 6" />
      <path d="M12 20v-9" />
      <path d="M6.53 9C4.6 8.8 3 7.1 3 5" />
      <path d="M6 13H2" />
      <path d="M3 21c0-2.1 1.7-3.9 3.8-4" />
      <path d="M20.97 5c0 2.1-1.6 3.8-3.5 4" />
      <path d="M22 13h-4" />
      <path d="M17.2 17c2.1.1 3.8 1.9 3.8 4" />
    </svg>
  );
}
