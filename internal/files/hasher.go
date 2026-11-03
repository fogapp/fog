package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"time"

	"gitea.lan/matthew/fog/internal/loop"
)

// The digest of every file is what makes downloads cacheable (strong
// ETags), duplicates visible, and backup verification honest. Uploads hash
// inline; everything that appeared by other means is hashed here, one
// file at a time in the background, so a dropped-in terabyte never
// competes with a listing for the disk.

// unhashable marks a file that could not be read (permissions, vanished
// mid-hash); it is not retried every pass. A change to the file clears the
// mark like any other digest.
const unhashable = "-"

func (m *Manager) kickHasher() {
	select {
	case m.hashKick <- struct{}{}:
	default:
	}
}

func (m *Manager) hashLoop(ctx context.Context) {
	const batch = 64
	loop.Run(ctx, 20*time.Second, m.hashKick, func(ctx context.Context) time.Duration {
		n, err := m.hashBatch(ctx, batch)
		if err != nil {
			m.Log.Warn("files hasher", "err", err)
		}
		if n == batch {
			return 50 * time.Millisecond // more waiting; keep going
		}
		return 30 * time.Second
	})
}

func (m *Manager) hashBatch(ctx context.Context, n int) (int, error) {
	ix, err := m.Index()
	if err != nil {
		return 0, err
	}
	root, err := m.openRoot()
	if err != nil {
		return 0, nil // no root yet: nothing to hash
	}
	nodes, err := ix.UnhashedBatch(n)
	if err != nil {
		return 0, err
	}
	for _, node := range nodes {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		sha, err := hashFile(root, node)
		if err != nil {
			sha = unhashable
		}
		if err := ix.SetHash(node.Path, node.Size, node.mtime, sha); err != nil {
			return 0, err
		}
	}
	return len(nodes), nil
}

// hashFile digests one file, refusing to report a digest for bytes that
// changed underneath it.
func hashFile(root *os.Root, n Node) (string, error) {
	f, info, err := openFileNF(root, n.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info.Size() != n.Size || info.ModTime().UnixNano() != n.mtime {
		return "", os.ErrInvalid // the listing will re-row it; hash next pass
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil || after.Size() != n.Size || after.ModTime().UnixNano() != n.mtime {
		return "", os.ErrInvalid
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Kick wakes the hasher: an owning subsystem just placed files.
func (m *Manager) Kick() { m.kickHasher() }
