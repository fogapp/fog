// Files across peers: a subscription that has stopped syncing, or one
// waiting on the operator, is a configuration state the Connect page
// shows. Filing it here keeps it visible between failures, with the fix a
// click away — and, because a sweep re-derives it, it clears itself when
// the peer recovers or the operator acknowledges. This is the only Inbox
// row for either state; filesfed used to file a notice saying the same
// thing beside it.
package issues

import (
	"context"
	"strconv"
	"time"

	"gitea.lan/matthew/fog/internal/store"
)

// filesSyncStale is how long a subscription may fail before it is an issue
// rather than a blip.
const filesSyncStale = 24 * time.Hour

// checkFilesSync files one issue per subscription that is failing for a
// day, and an info per subscription parked on a widened offer. Scope is
// the host: subscriptions are fog's, not an app's.
func checkFilesSync(_ context.Context, sn *snapshot) report {
	rep := report{evaluated: []int64{0}}
	now := sn.now
	if now.IsZero() {
		now = time.Now()
	}
	for _, sub := range sn.fileSubs {
		peer := sn.peerNames[sub.PeerID]
		if peer == "" {
			peer = "the peer"
		}
		subject := strconv.FormatInt(sub.ID, 10)
		switch sub.Status {
		case "error":
			last := sub.LastSyncedAt
			if last == "" {
				last = sub.CreatedAt
			}
			t, err := time.Parse(time.RFC3339, last)
			if err != nil || now.Sub(t) < filesSyncStale {
				continue
			}
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "files-sync-failing", Subject: subject, Severity: "warning",
				Title:  sub.Title + " from " + peer + " has not synced for a day",
				Detail: "Every pull since " + t.Format("2 Jan 15:04") + " has failed (last error: " + sub.LastError + "). The copy in Drive/" + sub.Folder + " is falling behind; fog keeps retrying with backoff.",
				Fix:    "Check the peer's address on the Connect page, or that they still share this folder. Sync now on the peer's card retries at once.",
				FixURL: "/federation",
			})
		case "offer_changed":
			rep.findings = append(rep.findings, store.IssueFinding{
				Kind: "files-offer-changed", Subject: subject, Severity: "info",
				Title:  peer + " widened " + sub.Title + " — waiting for you",
				Detail: "The share now says: " + sub.CurrentImpact + " Nothing is pulled until the new terms are acknowledged.",
				Fix:    "Read the new terms on the peer's card and acknowledge them, or remove the copy.",
				FixURL: "/federation",
			})
		}
	}
	return rep
}
