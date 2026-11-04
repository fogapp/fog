package offsite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/store"
)

// The Files tree goes offsite as file-level content-addressed blobs plus a
// sealed manifest per snapshot. The local snapshot's .fog-snapshot.json
// already lists every file with its digest, so what to upload is decided
// from the manifest and the copy rows alone: a blob fog has stored (and
// not hidden) is not read again. Blobs before the manifest, always — a
// manifest never names what is not there.

// snapshotManifest is the slice of files' .fog-snapshot.json the sync
// reads (the files package keeps the full type).
type snapshotManifest struct {
	Format    string `json:"format"`
	CreatedAt string `json:"created_at"`
	Mode      string `json:"mode"`
	Root      string `json:"root"`
	Files     []struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		Mtime  int64  `json:"mtime"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
	Excluded []string `json:"excluded,omitempty"`
	Bytes    int64    `json:"bytes"`
}

const filesManifestName = ".fog-snapshot.json"

func readSnapshotManifest(dir string) (*snapshotManifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, filesManifestName))
	if err != nil {
		return nil, err
	}
	var mf snapshotManifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, err
	}
	return &mf, nil
}

// stampOfSnapshot is a Files snapshot's stamp: its directory name for a
// hardlink snapshot; the creation time for the rolling mirror.
func stampOfSnapshot(row *store.FileSnapshot) string { return stampOf(row.Path, row.CreatedAt, row.ID) }

// syncFiles copies every recorded Files snapshot the target lacks.
func (m *Manager) syncFiles(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress) error {
	rows, err := m.Store.ListFileSnapshots()
	if err != nil {
		return err
	}
	ix, err := m.copyIndex(t, store.OffsiteKindFilesSnapshot)
	if err != nil {
		return err
	}
	// Blob rows, read the first time a snapshot needs uploading and kept
	// current across snapshots as blobs land.
	var blobs copyIndex
	var errs []string
	for i := range rows {
		row := &rows[i]
		subject := strconv.FormatInt(row.ID, 10)
		if ix.done(subject) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !m.available(row.Path) {
			m.Log.Warn("offsite: files snapshot is on an unavailable share; skipped", "snapshot", row.ID)
			continue
		}
		if row.StoreSnapshot != "" && m.ChunkStore != nil {
			if err := m.syncFilesStore(ctx, t, r, up, p, ix, row, subject); err != nil {
				errs = append(errs, "files "+stampOfSnapshot(row)+": "+err.Error())
			}
			continue
		}
		mf, err := readSnapshotManifest(row.Path)
		if err != nil {
			continue // pruned locally, or a snapshot still being written
		}
		if blobs == nil {
			if blobs, err = m.copyIndex(t, store.OffsiteKindBlob); err != nil {
				return err
			}
		}
		if err := m.syncFilesSnapshot(ctx, t, r, up, p, ix, blobs, row, mf, subject); err != nil {
			errs = append(errs, "files "+stampOfSnapshot(row)+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (m *Manager) syncFilesSnapshot(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress,
	ix, blobs copyIndex, row *store.FileSnapshot, mf *snapshotManifest, subject string) error {
	stamp := stampOfSnapshot(row)
	// Decide what to upload from the manifest and the copy rows.
	snap := FilesSnapshot{Format: Format, Stamp: stamp, CreatedAt: mf.CreatedAt, Root: mf.Root, Excluded: mf.Excluded, SourceSnapshotID: row.ID}
	var todo []pendingBlob
	seen := map[string]bool{}
	var total int64
	for _, f := range mf.Files {
		if f.SHA256 == "" {
			continue // torn at snapshot time; the local manifest says so
		}
		blob, err := r.Keys.BlobIDForDigest(f.SHA256)
		if err != nil {
			return err
		}
		snap.Files = append(snap.Files, FileEntry{Path: f.Path, Size: f.Size, Mtime: f.Mtime, SHA256: f.SHA256, Blob: blob})
		snap.Bytes += f.Size
		snap.Count++
		if blobs[blob].State == store.OffsiteStored || seen[blob] {
			continue
		}
		seen[blob] = true
		todo = append(todo, pendingBlob{path: f.Path, blob: blob, sha: f.SHA256, size: f.Size})
		total += f.Size
	}
	p.Total(total)
	if err := m.uploadBlobs(ctx, t, r, up, p, blobs, stamp, row.Path, todo); err != nil {
		return err
	}
	doc, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	key := r.Layout.FilesSnapshot(stamp)
	sealed, err := r.Keys.SealBytes(r.Layout.Name(key), doc)
	if err != nil {
		return err
	}
	if _, err := r.Client.PutBytes(ctx, key, sealed, withContentType(up.Lock, "application/octet-stream")); err != nil {
		return fmt.Errorf("writing the manifest: %w", err)
	}
	summary, err := json.Marshal(filesIndexLine(&snap))
	if err != nil {
		return err
	}
	if err := m.stored(ix, store.OffsiteCopy{TargetID: t.ID, Kind: store.OffsiteKindFilesSnapshot, Subject: subject, Key: key,
		Bytes: int64(len(sealed)), Doc: string(doc), Summary: string(summary), UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)}); err != nil {
		return err
	}
	m.Log.Info("offsite: files snapshot copied", "target", t.Name, "stamp", stamp, "files", snap.Count, "uploaded", len(todo))
	return nil
}

// pendingBlob is one file whose content the target lacks.
type pendingBlob struct {
	path, blob, sha string
	size            int64
}

// blobWorkers is how many blobs go up at once. A Drive is mostly small
// files, where a lone upload spends its time waiting on round trips; a
// few in flight fill the link, and the rate limiter they share still caps
// the total.
var blobWorkers = 6

// blobBatch is how many landed blobs are recorded per store write.
const blobBatch = 64

// uploadBlobs sends todo's blobs, blobWorkers at a time, and records each
// as stored. Rows are written in batches; every one is written before it
// returns — the snapshot's manifest, written next, must never name a blob
// the copy rows don't know. A crash before a batch is written costs only
// a re-upload of those blobs, which content addressing makes harmless.
func (m *Manager) uploadBlobs(ctx context.Context, t *store.OffsiteTarget, r *Reader, up *Uploader, p *jobs.Progress,
	blobs copyIndex, stamp, dir string, todo []pendingBlob) error {
	var (
		mu      sync.Mutex // blobs, landed, started
		landed  []store.OffsiteCopy
		started int
	)
	flush := func() error {
		if err := m.Store.UpsertOffsiteCopies(landed); err != nil {
			return err
		}
		for _, c := range landed {
			blobs[c.Subject] = c
		}
		landed = landed[:0]
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(blobWorkers)
	for _, item := range todo {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			key := r.Layout.Blob(item.blob)
			mu.Lock()
			started++
			p.Current(fmt.Sprintf("Drive %s · %d of %d files · %s", stamp, started, len(todo), item.path))
			state := blobs.resume(item.blob)
			mu.Unlock()
			save := m.saver(t, blobs, store.OffsiteKindBlob, item.blob, key)
			src := FileSource(filepath.Join(dir, filepath.FromSlash(item.path)))
			res, err := up.Upload(gctx, key, r.Layout.Name(key), src, &state, func(st *UploadState) error {
				mu.Lock()
				defer mu.Unlock()
				return save(st)
			})
			if err != nil {
				return fmt.Errorf("%s: %w", item.path, err)
			}
			// The rolling mirror is live files, not a frozen hardlink
			// snapshot, so a blob's content can drift from the digest the
			// manifest named it by. What was sealed and sent must match, or a
			// reader checking the content against the manifest later would
			// reject it — catch it here instead of storing a mismatched blob.
			if item.sha != "" && !strings.EqualFold(res.SHA256, item.sha) {
				return fmt.Errorf("%s: content changed under the mirror (uploaded sha256 %s, manifest %s)", item.path, res.SHA256, item.sha)
			}
			mu.Lock()
			defer mu.Unlock()
			landed = append(landed, store.OffsiteCopy{TargetID: t.ID, Kind: store.OffsiteKindBlob, Subject: item.blob, Key: key, Bytes: res.Bytes,
				State: store.OffsiteStored, UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)})
			p.Add(0, 1)
			if len(landed) >= blobBatch {
				return flush()
			}
			return nil
		})
	}
	err := g.Wait()
	// What landed before a failure is still worth recording.
	mu.Lock()
	defer mu.Unlock()
	if ferr := flush(); err == nil {
		err = ferr
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// sweepBlobs hides every stored blob no retained Files snapshot names.
// Runs after the snapshot series has had retention applied, from the
// manifests fog holds locally; a blob hidden here is a reversible marker
// like everything else, and a later snapshot that needs it again simply
// uploads it again.
//
// A blob that landed after the newest manifest is spared: blobs go
// up before their manifest, so it most likely belongs to a snapshot whose
// upload failed part way, and sweeping it would throw away the progress
// the next run resumes from.
//
// stored is the target's stored copies; it returns them less the blobs it
// hid.
func (m *Manager) sweepBlobs(ctx context.Context, t *store.OffsiteTarget, r *Reader, stored []store.OffsiteCopy) ([]store.OffsiteCopy, error) {
	hasBlobs := false
	for _, c := range stored {
		if c.Kind == store.OffsiteKindBlob {
			hasBlobs = true
			break
		}
	}
	if !hasBlobs {
		// Nothing to sweep, so no manifest needs reading.
		return stored, nil
	}
	snaps, err := m.Store.ListOffsiteCopies(t.ID, store.OffsiteKindFilesSnapshot, store.OffsiteStored)
	if err != nil {
		return stored, err
	}
	// Hidden manifests count for "newest": a series retention has emptied
	// still dates the blobs uploaded before it.
	all, err := m.Store.ListOffsiteCopiesSlim(t.ID, store.OffsiteKindFilesSnapshot)
	if err != nil {
		return stored, err
	}
	newest := ""
	for _, c := range all {
		newest = max(newest, c.UploadedAt)
	}
	referenced := map[string]bool{}
	for _, c := range snaps {
		var snap FilesSnapshot
		if json.Unmarshal([]byte(c.Doc), &snap) != nil {
			// A manifest fog cannot read must not cause a sweep of what it
			// might name.
			return stored, fmt.Errorf("files snapshot copy %s has an unreadable manifest; blobs not swept", c.Subject)
		}
		for _, f := range snap.Files {
			referenced[f.Blob] = true
		}
	}
	var errs []string
	hidden := map[int64]bool{}
	for _, c := range stored {
		if c.Kind != store.OffsiteKindBlob || referenced[c.Subject] || c.UploadedAt > newest {
			continue
		}
		if err := r.Client.Delete(ctx, c.Key); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		m.Store.SetOffsiteCopyState(c.ID, store.OffsiteHidden)
		hidden[c.ID] = true
	}
	kept := withoutHidden(stored, hidden)
	if len(errs) > 0 {
		return kept, errors.New(strings.Join(errs, "; "))
	}
	return kept, nil
}
