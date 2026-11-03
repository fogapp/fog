// Flare ingest: the Sentry-envelope-compatible error endpoint managed apps
// (and, one day, browsers and peer fogs) report to. Lives under /ingest —
// outside /api — so session auth, the Origin guard, and request logging
// never apply; the DSN's path prefix (FLARE_DSN=http://key@host/ingest/<id>)
// is what steers official SDKs here. See clients/flare/README.md.
package server

import (
	"compress/gzip"
	"crypto/subtle"
	"io"
	"net/http"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/flare"
)

// The compressed body cap matches readJSON's 1MiB; the decompressed cap
// guards against gzip bombs (Sentry's own limit is 1MiB per event item, and
// envelopes here carry a handful of items at most).
const (
	maxIngestBody         = 1 << 20
	maxIngestDecompressed = 8 << 20
)

func (s *Server) handleFlareIngest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "app")
	if !ok {
		return
	}
	app, err := s.store.AppByID(id)
	if err != nil {
		// 404, not 403: same as the webhook, an unauthenticated caller
		// probing project ids learns nothing about which exist.
		notFound(w, "not found")
		return
	}

	var body io.Reader = http.MaxBytesReader(w, r.Body, maxIngestBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			badRequest(w, "invalid gzip body")
			return
		}
		defer gz.Close()
		body = io.LimitReader(gz, maxIngestDecompressed)
	}
	env, err := flare.ParseEnvelope(body)
	if err != nil {
		badRequest(w, "invalid envelope: "+err.Error())
		return
	}

	key := flare.ExtractKey(r.Header, r.URL.Query(), env.Header)
	if key == "" || !s.checkErrorsKey(app.ID, key) {
		// 403 is load-bearing: the Sentry SDK spec stops a client that gets
		// it, where a 401 would be retried.
		forbidden(w, "invalid key")
		return
	}

	// Back-pressure: official SDKs honor Retry-After by shedding load
	// client-side, so a crash-looping app silences itself instead of
	// hammering the box. Counted per envelope — batches stay cheap.
	if !limit(w, s.flareLimiter, app.Name, "rate limited") {
		return
	}

	eventID := ""
	for _, item := range env.Items {
		if item.Type != "event" {
			// Sessions, transactions, client reports: legal to send, not
			// stored here. Dropping them silently is what the spec expects.
			continue
		}
		id, err := s.flareProc.IngestEvent(app, item.Payload)
		if err != nil {
			s.log.Warn("flare ingest", "app", app.Name, "err", err)
			continue
		}
		eventID = id
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": eventID})
}

// checkErrorsKey validates a DSN key against the app's vault-stored
// _errors_key. Reads the store directly rather than going through the
// engine: the check needs no minting, and the handler stays testable with
// the same store+vault harness as everything else.
func (s *Server) checkErrorsKey(appID int64, key string) bool {
	want, err := s.secrets().Get(appID, apps.SecretErrorsKey)
	return err == nil && want != "" &&
		subtle.ConstantTimeCompare([]byte(want), []byte(key)) == 1
}
