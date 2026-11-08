//go:build linux

package files

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A name an app turns into a FIFO (or a device node) between fog's check
// and its open used to be opened all the same — and opening some devices
// does something by itself. Now the name is held O_PATH, checked, and only
// a regular file or folder is ever really opened: a FIFO's writer, waiting
// for a reader, is never released.
func TestOpenNeverOpensASpecialFile(t *testing.T) {
	t.Parallel()

	m := newManager(t)
	root, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(m.Root(), "pipe")
	if err := unix.Mkfifo(fifo, 0o666); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0) // blocks until a reader opens
		if err == nil {
			f.Close()
		}
		close(released)
	}()
	if _, err := openNF(root, "pipe", os.O_RDONLY, 0); !errors.Is(err, ErrBadName) {
		t.Fatalf("openNF on a FIFO: %v", err)
	}
	if _, _, err := openFileNF(root, "pipe"); !errors.Is(err, ErrBadName) {
		t.Fatalf("openFileNF on a FIFO: %v", err)
	}
	select {
	case <-released:
		t.Fatal("fog opened the FIFO")
	case <-time.After(300 * time.Millisecond):
	}
	r, _ := os.OpenFile(fifo, os.O_RDONLY|unix.O_NONBLOCK, 0) // let the writer go
	<-released
	r.Close()

	// A create where a FIFO appears instead of the free name is not opened
	// either: the create is O_EXCL, and the FIFO is then checked.
	if _, err := openNF(root, "pipe", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644); !errors.Is(err, ErrBadName) {
		t.Fatalf("create over a FIFO: %v", err)
	}
	// A regular file still opens, and creates still create.
	write(t, m, "plain.txt", "x")
	if f, err := openNF(root, "plain.txt", os.O_RDWR, 0); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if f, err := openNF(root, "new.txt", os.O_WRONLY|os.O_CREATE, 0o644); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
}
