// Acting on a person's behalf without their session. An app behind the gate
// holds a Fog-Assertion for the person using it right now; presenting it
// alongside its own FOG_API_TOKEN proves both who the app is and that this
// person is at its door this minute. That pair, never a fog session, is
// what an app spends to ask fog something on a person's behalf.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/store"
)

// assertionSkew tolerates clocks a little apart between fogd and an app.
const assertionSkew = 5 * time.Second

var errNotPresent = errors.New("no valid Fog-Assertion for this app")

// presentPerson verifies the request's Fog-Assertion: signed by fog, for one
// of app's own gated hosts, unexpired, naming an active person of this
// household.
func (s *Server) presentPerson(app *store.App, r *http.Request) (*store.Person, error) {
	raw := r.Header.Get("Fog-Assertion")
	if raw == "" {
		return nil, errNotPresent
	}
	payload, err := s.keys.VerifyAs(raw, "JWT")
	if err != nil {
		return nil, errNotPresent
	}
	var c struct {
		Iss string `json:"iss"`
		Aud string `json:"aud"`
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
		Iat int64  `json:"iat"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return nil, errNotPresent
	}
	now := time.Now()
	if now.After(time.Unix(c.Exp, 0).Add(assertionSkew)) || time.Unix(c.Iat, 0).After(now.Add(assertionSkew)) {
		return nil, errNotPresent
	}
	// The same rule as /app/v1/gate and fog-client (gate_handlers.go): the
	// issuer exactly, and an audience among this app's own, scheme and all.
	if c.Iss != s.gateIssuer() || !slices.Contains(s.gateAudiences(app.ID), c.Aud) {
		return nil, errNotPresent
	}
	inst, err := s.fed.Identity()
	if err != nil {
		return nil, err
	}
	uid, ok := strings.CutPrefix(c.Sub, identity.Address(inst.InstanceID, ""))
	if !ok || uid == "" {
		return nil, errNotPresent
	}
	p, err := s.store.PersonByUID(uid)
	if err != nil || p.Status != store.PersonStatusActive {
		return nil, errNotPresent
	}
	return p, nil
}

// handleAppFilesSearch searches Drive for the operator, asked by an app the
// operator is using right now (newtab's search box). Three things must
// hold: the app is granted "drive" in its uses (the operator ticks Search
// Drive for you under its Config, App access); the person at its door this
// minute is the operator, proven by a fresh Fog-Assertion; and the answer
// is only what such an app may see — names, paths, sizes, dates and types,
// never digests, and never another app's own folder (Apps/<other>), a
// peer's files (Peers), or anything hidden.
func (s *Server) handleAppFilesSearch(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	// Default-deny: an app with no spec to read is not granted anything.
	_, spec, err := apps.SpecAt(s.store, app.ID, 0)
	if err != nil || !spec.UsesGrant(apps.UsesDrive) {
		forbidden(w, `searching Drive needs "drive" in this app's uses`)
		return
	}
	p, err := s.presentPerson(app, r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if p.Role != store.PersonRoleOperator {
		forbidden(w, "Drive is the operator's")
		return
	}
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if utf8.RuneCountInString(query) < appSearchMinQuery {
		writeJSON(w, http.StatusOK, map[string]any{"entries": []appFileHit{}})
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > appSearchMaxLimit {
		limit = appSearchMaxLimit
	}
	nodes, err := s.files.SearchScoped(query, limit, appSearchScope(app.Name, spec, s.files.Root()))
	if err != nil {
		filesError(w, err)
		return
	}
	out := make([]appFileHit, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, appFileHit{Path: n.Path, Name: n.Name, Kind: n.Kind, Size: n.Size, Modified: n.Modified, Mime: n.Mime})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// An app's Drive search answers at most appSearchMaxLimit entries, and
// nothing for a query shorter than appSearchMinQuery: one letter is a
// listing, not a search.
const (
	appSearchMinQuery = 2
	appSearchMaxLimit = 50
)

// appFileHit is one search result as an app sees it: what a search box
// shows and links to, and nothing else the index knows (digests, inodes,
// who mounts or shares what).
type appFileHit struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Kind     string `json:"kind" ts:"\"file\" | \"dir\""`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Mime     string `json:"mime,omitempty"`
}

// appSearchScope is what an app searching Drive may see: everything but
// the app folders of other apps and the copies of peers' shares (a peer
// decides whether apps may read its files, through a mount, not here) —
// except the folders this app mounts itself, which it can read anyway.
func appSearchScope(name string, spec *apps.AppSpec, root string) files.SearchScope {
	sc := files.SearchScope{
		Hide:    []string{files.AppsDir, filesfed.PeersFolder},
		Show:    []string{apps.AppDataFolder(name)},
		HideDot: true,
	}
	for _, v := range spec.AllVolumes() {
		hp := v.HostPath(root)
		if hp == "" || root == "" {
			continue
		}
		rel, err := filepath.Rel(root, hp)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if clean, err := files.CleanRel(filepath.ToSlash(rel)); err == nil && clean != "" && !files.IsUnder(clean, filesfed.PeersFolder) {
			sc.Show = append(sc.Show, clean)
		}
	}
	return sc
}
