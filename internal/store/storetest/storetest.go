// Package storetest opens fog stores for tests without paying for the
// migrations every time.
//
// A fresh store.Open replays every migration, which costs ~40ms normally
// and well over a second under -race: modernc.org/sqlite is SQLite
// translated into Go, so the race detector instruments all of it. With
// hundreds of tests each opening a store, that one cost dominated the whole
// suite. Open here migrates a database once per test process, keeps its
// bytes, and gives each new path a copy, so a test's store is exactly what
// store.Open makes on a fresh path, for ~30ms.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"gitea.lan/matthew/fog/internal/store"
)

// migrated is the bytes of a freshly migrated database, built on first
// use. It is a VACUUM INTO copy, so it is one self-contained file with no
// WAL beside it.
var migrated = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "fog-storetest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "migrate.db"))
	if err != nil {
		return nil, fmt.Errorf("migrating the template store: %w", err)
	}
	defer st.Close()
	snap := filepath.Join(dir, "template.db")
	if err := st.SnapshotTo(context.Background(), snap); err != nil {
		return nil, fmt.Errorf("snapshotting the template store: %w", err)
	}
	return os.ReadFile(snap)
})

// Open is store.Open for tests. A path that doesn't exist yet starts as a
// copy of an already-migrated database; a path that does exist (a test
// reopening its store, or one that planted an old schema to upgrade) is
// opened as it is, exactly as store.Open would.
func Open(path string) (*store.Store, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		b, err := migrated()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	}
	return store.Open(path)
}
