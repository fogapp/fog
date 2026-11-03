package notify

import (
	"sync"

	"gitea.lan/matthew/fog/internal/store"
)

// Latch is one persistent notice that a condition either holds or doesn't:
// raised on the crossing into it, dismissed on the crossing out, so a
// check that runs on every tick never stacks duplicates or rewrites the
// notice it already shows. Until the first Raise or Clear the state is
// unknown — the notice may be a leftover from the previous process — so
// the first Clear dismisses whatever is there.
type Latch struct {
	Store *store.Store
	Kind  string
	Key   string // the notice's app scope; "" for instance-wide notices

	// mu also covers the store write, so a Raise and a Clear racing from
	// two goroutines leave the notice matching the flag.
	mu    sync.Mutex
	known bool
	up    bool
}

// Raise puts the notice up unless it already is, and reports whether it
// did — the caller's cue to log or push once per crossing.
func (l *Latch) Raise(title, body string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.known && l.up {
		return false
	}
	l.known, l.up = true, true
	l.Store.CreateNotice(l.Kind, l.Key, title, body)
	return true
}

// Clear takes the notice down if it is up, or might be.
func (l *Latch) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.known && !l.up {
		return
	}
	l.known, l.up = true, false
	l.Store.DismissNoticeByKind(l.Kind, l.Key)
}
