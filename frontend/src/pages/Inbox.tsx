// The Inbox: one page answering "does anything need me?", shaped like mail
// — tabs across the top, what arrived down the left, the selected thing
// open beside it. Issues lead — what the scanner found, headed by notices,
// the steps only a person can take; errors the apps reported (Flare)
// follow; Feedback holds what people
// wrote from the header's bug button, here or on a peer; Resolved is the archive of
// issues a human silenced or the world stopped reproducing.
//
// The page owns the viewport the way Files does — the columns scroll, the
// page does not — so the tab strip stays put while a long list moves.

import { useEffect } from "react";
import { useSearchParams } from "react-router-dom";
import Subheader from "../components/Subheader";
import { useTabParam } from "../components/Tabs";
import { INBOX_TABS, INBOX_TAB_LABELS } from "../lib/paths";
import { useInboxCounts } from "../lib/useInbox";
import Errors from "./inbox/Errors";
import Feedback from "./inbox/Feedback";
import Issues from "./inbox/Issues";

export default function Inbox() {
  const [params, setParams] = useSearchParams();
  // A tab switch resets the tab-local params (?g=, ?sel=, ?origin=) —
  // carrying a selection into another tab would strand it.
  const [tab, setTab] = useTabParam(INBOX_TABS);
  // The Configuration tab was ?tab=config before it became Issues, and the
  // Issues sub-tabs were ?show=acked / ?show=resolved before they became
  // one Resolved tab. Old links keep landing somewhere true.
  useEffect(() => {
    // Notices became rows at the top of Issues; a link to one keeps its
    // selection under the prefixed id they carry there.
    if (params.get("tab") === "notices") {
      const sel = params.get("sel");
      setParams(sel ? { tab: "issues", sel: `n${sel}` } : { tab: "issues" }, { replace: true });
      return;
    }
    if (params.get("tab") === "config") {
      setParams({ tab: "issues" }, { replace: true });
      return;
    }
    const show = params.get("show");
    if (show === "acked" || show === "resolved") setParams({ tab: "resolved" }, { replace: true });
    else if (show) setParams({ tab: "issues" }, { replace: true });
  }, [params, setParams]);

  // Counts for every tab stay live regardless of which is open — the point
  // of the badges is seeing the other tabs' workload without clicking over.
  // Resolved carries none: an archive with a number on it reads as work.
  const counts = useInboxCounts();

  return (
    <div className="flex h-[calc(100vh-3.5rem)] min-h-[24rem] flex-col">
      {/* No page title: the breadcrumb overhead already reads
          "Inbox › Issues", and in a viewport-height layout every line
          spent repeating it is a line the list doesn't get. */}
      <Subheader
        label="Inbox view"
        active={tab}
        onChange={setTab}
        items={[
          { id: "issues", label: INBOX_TAB_LABELS.issues, count: counts.issues.total + counts.notices, hot: counts.issues.critical > 0 },
          { id: "errors", label: INBOX_TAB_LABELS.errors, count: counts.errors, hot: counts.errors > 0 },
          { id: "feedback", label: INBOX_TAB_LABELS.feedback, count: counts.feedback.open },
          { id: "resolved", label: INBOX_TAB_LABELS.resolved },
        ]}
      />

      {tab === "errors" && <Errors />}
      {tab === "issues" && <Issues />}
      {tab === "feedback" && <Feedback />}
      {tab === "resolved" && <Issues archive />}
    </div>
  );
}
