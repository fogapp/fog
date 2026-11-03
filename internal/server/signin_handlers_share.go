// A password-protected share link's password check (docs/signin-contract.md,
// "A share link's password"). A share's password is a credential like any
// other and a link is public, so guessing is limited — but by who guesses,
// never by locking the link, so whoever holds the link and its password is
// never cut off by someone else's guesses:
//
//   - each address (an IPv6 /64) has 10 wrong passwords per link per 15
//     minutes, then 429 until the window ends;
//   - an address with 50 wrong share passwords in a day, at any links, is
//     locked out of share passwords (423) for the rest of the day;
//   - a link guessed at more than 50 times in a day tells the operator, once.
//
// A right password earns a share cookie for that link (an hour, held only in
// this process), so the downloads and range requests that follow skip the
// hash; and a check waits briefly for its place in the hashing line rather
// than being refused at once.
package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/auth"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	// shareFailures is wrong passwords per link per address per
	// failureWindow.
	shareFailures = 10
	// shareStrikes is wrong share passwords per address per day; past it
	// the address is locked out of share passwords for the day.
	shareStrikes = 50
	shareDay     = 24 * time.Hour
	// shareAlarm is wrong passwords per link per day before the operator is
	// told (shareAlarmKind).
	shareAlarm     = 50
	shareAlarmKind = "share-password-guessed"
	// minSharePassword is the shortest share password fog accepts.
	minSharePassword = 8
	// shareGrantTTL is how long a right password's share cookie lasts.
	shareGrantTTL = time.Hour
	// maxShareGrants bounds the grants held per link; past it a new grant
	// replaces that link's oldest, so no one can fill the table for
	// another link — or at all.
	maxShareGrants = 200
	// shareWait is how long a check waits for its place in the line.
	shareWait = 2 * time.Second
)

// shareGrant is a right password's grant for one link, as its password was.
type shareGrant struct {
	digest string // of the cookie's token
	hash   string // digest of the password hash it was earned against
	exp    time.Time
}

// shareGrants are the grants this process holds, per link, oldest first.
type shareGrants struct {
	mu sync.Mutex
	m  map[int64][]shareGrant
}

func shareKey(sh *store.FileShare) string { return "share:" + strconv.FormatInt(sh.ID, 10) }

func shareCookieName(sh *store.FileShare) string { return "fog_share_" + strconv.FormatInt(sh.ID, 10) }

// sharePasswordOK checks a request for a protected link, answering the
// request itself when it is not let through.
func (s *Server) sharePasswordOK(w http.ResponseWriter, r *http.Request, sh *store.FileShare) bool {
	if s.shareGranted(r, sh) {
		return true
	}
	l := s.signinLimits()
	addr, _ := s.limitAddr(r)
	if l.shareStrikes.Exhausted(addr) {
		http.Error(w, "Too many wrong share passwords from here today; try again tomorrow.", http.StatusLocked)
		return false
	}
	perLink := shareKey(sh) + "@" + addr
	if l.shareFailures.Exhausted(perLink) {
		w.Header().Set("Retry-After", strconv.Itoa(int(failureWindow.Seconds())))
		http.Error(w, "Too many wrong passwords for this link; try again later.", http.StatusTooManyRequests)
		return false
	}
	_, pw, ok := r.BasicAuth()
	if ok {
		// In the one fair hashing line, as this address, in the share
		// links' own capped part of it, waiting briefly for a place.
		caller := s.shareCaller(r)
		caller.Wait = shareWait
		var err error
		ok, err = auth.CheckPasswordAs(pw, sh.PasswordHash(), caller)
		if err != nil {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Too many password checks at once; try again shortly.", http.StatusTooManyRequests)
			return false
		}
		if !ok {
			l.shareFailures.Allow(perLink)
			l.shareStrikes.Allow(addr)
			if !l.shareAlarms.Allow(shareKey(sh)) {
				s.alarmShare(sh)
			}
		}
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="shared files"`)
		http.Error(w, "This link needs a password.", http.StatusUnauthorized)
		return false
	}
	s.grantShare(w, r, sh)
	return true
}

// shareGranted says whether r carries a live grant for this link, earned
// against its current password.
func (s *Server) shareGranted(r *http.Request, sh *store.FileShare) bool {
	c, err := r.Cookie(shareCookieName(sh))
	if err != nil || c.Value == "" {
		return false
	}
	g := &s.signinLimits().shareGrants
	g.mu.Lock()
	defer g.mu.Unlock()
	digest, hash, now := auth.DigestToken(c.Value), auth.DigestToken(sh.PasswordHash()), time.Now()
	for _, got := range g.m[sh.ID] {
		if got.digest == digest {
			return got.hash == hash && now.Before(got.exp)
		}
	}
	return false
}

// grantShare hands a right password's share cookie for this link, scoped
// to the link's own path.
func (s *Server) grantShare(w http.ResponseWriter, r *http.Request, sh *store.FileShare) {
	g := &s.signinLimits().shareGrants
	now := time.Now()
	token := randx.Token()
	g.mu.Lock()
	if g.m == nil {
		g.m = map[int64][]shareGrant{}
	}
	// This link's live grants, the oldest dropped when it holds its most.
	live := g.m[sh.ID][:0:0]
	for _, v := range g.m[sh.ID] {
		if now.Before(v.exp) {
			live = append(live, v)
		}
	}
	if len(live) >= maxShareGrants {
		live = live[len(live)-maxShareGrants+1:]
	}
	g.m[sh.ID] = append(live, shareGrant{digest: auth.DigestToken(token), hash: auth.DigestToken(sh.PasswordHash()), exp: now.Add(shareGrantTTL)})
	g.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: shareCookieName(sh), Value: token, Path: "/s/" + r.PathValue("token"),
		HttpOnly: true, Secure: forwardedSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: int(shareGrantTTL.Seconds()),
	})
}

// alarmShare tells the operator a link is being guessed at, once a day.
func (s *Server) alarmShare(sh *store.FileShare) {
	if !s.signinLimits().shareAlarmed.Allow(shareKey(sh)) {
		return
	}
	title := "A share link's password is being guessed"
	body := "The link to " + sh.Path + " had more than " + strconv.Itoa(shareAlarm) +
		" wrong passwords today. Each address guessing is locked out after its own; whoever has the link and its password still gets in. If the link has gone further than you meant, delete it in Files."
	s.store.CreateNotice(shareAlarmKind, shareKey(sh), title, body)
	s.notify(title, body, false)
}
