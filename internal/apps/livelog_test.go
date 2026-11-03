package apps

import (
	"strings"
	"testing"
)

// A build that emits far more than the cap must not grow the buffer without
// bound, and must keep the end — the part that explains a failure.
func TestLiveLogIsBounded(t *testing.T) {
	l := &liveDeploy{}
	l.start()
	line := strings.Repeat("x", 999) + "\n"
	for i := 0; i < 2000; i++ { // ~2 MB through a 256 KB cap
		l.Write([]byte(line))
	}
	l.Write([]byte("FINAL LINE\n"))

	if got := len(l.log); got > maxLiveLog {
		t.Errorf("buffer is %d bytes, cap is %d", got, maxLiveLog)
	}
	// The backing array must be reclaimed too — a plain reslice would keep the
	// full 2 MB alive and defeat the point.
	if got := cap(l.log); got > 4*maxLiveLog {
		t.Errorf("backing array is %d bytes, far above the %d cap", got, maxLiveLog)
	}
	out := l.String()
	if !strings.HasSuffix(out, "FINAL LINE\n") {
		t.Error("the tail was dropped — the most useful part of a failing log")
	}
	if !strings.Contains(out, truncationNotice) {
		t.Error("truncated log does not say it was truncated")
	}
	if !strings.HasPrefix(out[len(truncationNotice):], "xxx") {
		t.Error("resumed mid-line rather than on a line boundary")
	}
}

// Below the cap, nothing is touched and nothing claims truncation.
func TestLiveLogUntruncated(t *testing.T) {
	l := &liveDeploy{}
	l.start()
	l.Write([]byte("hello\n"))
	l.setStage("building")
	l.Write([]byte("world\n"))

	out := l.String()
	if strings.Contains(out, truncationNotice) {
		t.Error("short log wrongly reports truncation")
	}
	for _, want := range []string{"hello\n", "==> building\n", "world\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q, got %q", want, out)
		}
	}
}

// A new operation starts from a clean slate, including the truncation flag —
// otherwise a short deploy following a huge one would claim to be trimmed.
func TestLiveLogResetsBetweenOperations(t *testing.T) {
	l := &liveDeploy{}
	l.start()
	l.Write([]byte(strings.Repeat("y", maxLiveLog+1024)))
	if !l.truncated {
		t.Fatal("expected the oversized write to set the truncation flag")
	}
	l.finish()

	l.start()
	l.Write([]byte("fresh\n"))
	out := l.String()
	if strings.Contains(out, truncationNotice) {
		t.Error("a new operation inherited the previous one's truncation flag")
	}
	if strings.Contains(out, "y") {
		t.Error("a new operation inherited the previous one's output")
	}
}
