package server

import (
	"sync"
	"time"
)

// filesNotifier coalesces Files change notices to apps per app and mount,
// and flushes them once changes go quiet for a moment.
type filesNotifier struct {
	mu      sync.Mutex
	pending map[string]filesNotice
	timer   *time.Timer
}

// queue records notices (keyed so a burst of changes under one mount is one
// notice) and (re)arms the quiet timer that runs flush.
func (f *filesNotifier) queue(notices map[string]filesNotice, quiet time.Duration, flush func()) {
	if len(notices) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		f.pending = map[string]filesNotice{}
	}
	for k, n := range notices {
		f.pending[k] = n
	}
	if f.timer == nil {
		f.timer = time.AfterFunc(quiet, flush)
	} else {
		f.timer.Reset(quiet)
	}
}

// take hands over everything pending and disarms the timer.
func (f *filesNotifier) take() map[string]filesNotice {
	f.mu.Lock()
	defer f.mu.Unlock()
	pending := f.pending
	f.pending, f.timer = nil, nil
	return pending
}

// dirNotifier remembers the last app-visible federation directory apps were
// told about, so a change no app could see (a heartbeat, a capability an
// app has no view of) doesn't wake every app up.
type dirNotifier struct {
	mu  sync.Mutex
	sum string
}

// changed records sum and reports whether it differs from the last one.
func (d *dirNotifier) changed(sum string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if sum == d.sum {
		return false
	}
	d.sum = sum
	return true
}
