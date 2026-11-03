package offsite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/jobs"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/store"
)

// Moving house: "I want to leave this provider" and "I want two clouds"
// are the same feature. Targets are a list; the nightly loop writes new
// objects to every active one; and a locker copy carries everything a
// target already holds into another, object by object, verified as it
// goes — a bit flip fails to open — under the same locker id, so nothing
// is re-encrypted and the destination becomes a second home for the very
// same locker. Retiring a target stops writes to it and keeps it
// readable; fog never empties a bucket.

var ErrCopyRunning = errors.New("a locker copy to this target is already running")

func copyKind(dest string) string { return "offsite-copy:" + dest }

// StartCopy copies src's locker into dst. dst must be empty of copies, or
// already be a home of the same locker (a resumed copy).
func (m *Manager) StartCopy(srcName, dstName string) (jobs.Job, error) {
	src, err := m.Store.OffsiteTargetByName(srcName)
	if err != nil {
		return jobs.Job{}, err
	}
	dst, err := m.Store.OffsiteTargetByName(dstName)
	if err != nil {
		return jobs.Job{}, err
	}
	if src.ID == dst.ID {
		return jobs.Job{}, errors.New("source and destination are the same target")
	}
	if dst.LockerID != src.LockerID {
		totals, err := m.Store.OffsiteCopyTotals(dst.ID)
		if err != nil {
			return jobs.Job{}, err
		}
		if totals.Stored > 0 || totals.Uploading > 0 {
			return jobs.Job{}, fmt.Errorf("%s already holds a locker of its own with %d objects — copy into an empty target, or forget and re-add it", dstName, totals.Stored)
		}
	}
	job, ok := m.jobs().StartExclusive(copyKind(dstName), "Copy locker "+srcName+" → "+dstName, func(ctx context.Context, p *jobs.Progress) error {
		err := m.copyLocker(ctx, p, src, dst)
		if err != nil {
			m.Log.Error("offsite copy", "from", srcName, "to", dstName, "err", err)
			return err
		}
		m.Store.Audit("system", "offsite.copy", dstName, fmt.Sprintf(`{"from":%q,"locker":%q}`, srcName, src.LockerID))
		return nil
	}, syncKind(dstName))
	if !ok {
		return jobs.Job{}, ErrCopyRunning
	}
	return job, nil
}

func (m *Manager) copyLocker(ctx context.Context, p *jobs.Progress, src, dst *store.OffsiteTarget) error {
	sr, err := m.Reader(ctx, src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src.Name, err)
	}
	dc, err := m.Client(dst)
	if err != nil {
		return err
	}
	dstLayout := Layout{Prefix: dst.Prefix, ID: src.LockerID}
	policy := ReadPolicy(m.Store)
	lock := m.lockOptions(dst, policy)
	up := &Uploader{Client: dc, Spool: m.spool(), Limit: NewRateLimiter(dst.MaxMbps), Lock: lock, Progress: func(n int64) { p.Add(n, 0) }}

	// 1. locker.json, as it is: same keys, same wraps.
	p.Current("locker.json")
	raw, _, err := sr.Client.GetBytes(ctx, sr.Layout.LockerJSON(), 1<<20)
	if err != nil {
		return err
	}
	if _, err := dc.PutBytes(ctx, dstLayout.LockerJSON(), raw, objstore.PutOptions{ContentType: "application/json"}); err != nil {
		return fmt.Errorf("writing locker.json to %s: %w", dst.Name, err)
	}

	// 2. every stored object src knows, oldest series first so a partial
	// copy is a coherent prefix of history.
	rows, err := m.Store.ListOffsiteCopies(src.ID, "", store.OffsiteStored)
	if err != nil {
		return err
	}
	var todo []store.OffsiteCopy
	var total int64
	have := map[string]bool{}
	if dstRows, err := m.Store.ListOffsiteCopiesSlim(dst.ID, "", store.OffsiteStored); err == nil {
		for _, c := range dstRows {
			have[c.Kind+"/"+c.Subject] = true
		}
	}
	for _, c := range rows {
		if have[c.Kind+"/"+c.Subject] {
			continue
		}
		todo = append(todo, c)
		total += c.Bytes
	}
	// Blobs before manifests, snapshots before rows would matter for a
	// reader of a half-copied locker; kinds sort so blobs (files-blob)
	// come before files-snapshot, and app snapshots carry their rows.
	sort.SliceStable(todo, func(i, j int) bool { return kindOrder(todo[i].Kind) < kindOrder(todo[j].Kind) })
	p.Total(total)
	var errs []string
	for i, c := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.Current(fmt.Sprintf("%d of %d · %s", i+1, len(todo), sr.Layout.Name(c.Key)))
		if err := m.copyObject(ctx, sr, up, dstLayout, &c, dst); err != nil {
			errs = append(errs, sr.Layout.Name(c.Key)+": "+err.Error())
			continue
		}
		p.Add(0, 1)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}

	// 3. The newest index, so a restore wizard sees the destination whole.
	var indexes []string
	sr.Client.List(ctx, sr.Layout.IndexPrefix(), func(o objstore.Object) error { indexes = append(indexes, o.Key); return nil })
	if len(indexes) > 0 {
		sort.Sort(sort.Reverse(sort.StringSlice(indexes)))
		raw, _, err := sr.Client.GetBytes(ctx, indexes[0], maxDocument)
		if err == nil {
			dc.PutBytes(ctx, dstLayout.Root()+sr.Layout.Name(indexes[0]), raw, lock)
		}
	}

	// 4. The destination is now a home of this locker. Its own former
	// locker (created empty when the target was added) is hidden so a
	// restore-from-nothing finds one locker under the prefix.
	if dst.LockerID != src.LockerID {
		old := Layout{Prefix: dst.Prefix, ID: dst.LockerID}
		dc.Delete(ctx, old.LockerJSON())
		if err := m.Store.SetOffsiteTargetLocker(dst.Name, src.LockerID); err != nil {
			return err
		}
	}
	m.forgetRing(dst.Name)
	m.forgetIndex(dst.Name)
	m.Store.SetOffsiteTargetSync(dst.Name, true, "")
	m.Log.Info("offsite: locker copied", "from", src.Name, "to", dst.Name, "objects", len(todo), "bytes", total)
	return nil
}

func kindOrder(kind string) int {
	switch kind {
	case store.OffsiteKindKit:
		return 0
	case store.OffsiteKindBlob:
		return 1
	case store.OffsiteKindFilesSnapshot:
		return 2
	case store.OffsiteKindApp:
		return 3
	}
	return 4
}

// copyObject moves one copy — every object it owns, an app backup's row
// after its snapshot so a reader never finds a row without one. Each
// object's sealed bytes are downloaded to the spool and opened end to end
// as they arrive, to prove they authenticate (a kit is checked for its
// magic instead), then uploaded as they are, straight from the downloaded
// file, to the same name in the destination's locker. The destination's
// copy row mirrors the source's.
func (m *Manager) copyObject(ctx context.Context, sr *Reader, up *Uploader, dstLayout Layout, c *store.OffsiteCopy, dst *store.OffsiteTarget) error {
	if err := os.MkdirAll(up.Spool, 0o700); err != nil {
		return err
	}
	var size int64
	for _, key := range sr.Layout.ObjectsOf(c) {
		n, err := copyOne(ctx, sr, up, dstLayout, key, c.Kind)
		if err != nil {
			return err
		}
		size += n
	}
	return m.Store.UpsertOffsiteCopy(store.OffsiteCopy{TargetID: dst.ID, Kind: c.Kind, Subject: c.Subject, Key: dstLayout.Root() + sr.Layout.Name(c.Key), Bytes: size,
		State: store.OffsiteStored, Doc: c.Doc, Summary: c.Summary, UploadedAt: m.now().UTC().Format(time.RFC3339), RetainUntil: retainUntil(up.Lock)})
}

// copyOne moves one object of a copy and returns the bytes uploaded.
func copyOne(ctx context.Context, sr *Reader, up *Uploader, dstLayout Layout, key, kind string) (int64, error) {
	tmp, err := os.CreateTemp(up.Spool, "copy-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	name := sr.Layout.Name(key)
	if err := downloadChecked(ctx, sr, key, kind, name, tmp); err != nil {
		return 0, err
	}
	var state UploadState
	res, err := up.Upload(ctx, dstLayout.Root()+name, name, FileSource(tmpName), &state, func(*UploadState) error { return nil })
	if err != nil {
		return 0, err
	}
	return res.Bytes, nil
}

// downloadChecked writes the sealed bytes at key into tmp (and closes it).
// A sealed object is authenticated in the same pass, through a pipe to an
// opener; a kit, which fog does not seal, is checked for its magic.
func downloadChecked(ctx context.Context, sr *Reader, key, kind, name string, tmp *os.File) error {
	rc, _, err := sr.Client.Get(ctx, key, "")
	if err != nil {
		tmp.Close()
		return err
	}
	defer rc.Close()
	if kind == store.OffsiteKindKit {
		_, err = io.Copy(tmp, rc)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		head := make([]byte, 8)
		f, err := os.Open(tmp.Name())
		if err != nil {
			return err
		}
		n, _ := io.ReadFull(f, head)
		f.Close()
		if !backup.IsKit(head[:n]) {
			return errors.New("the kit in the source bucket is not a fog recovery kit")
		}
		return nil
	}
	pr, pw := io.Pipe()
	authed := make(chan error, 1)
	go func() {
		plain, err := sr.Keys.Open(pr, name)
		if err == nil {
			_, err = io.Copy(io.Discard, plain)
		}
		// A refusal stops the download too: the writer side sees it.
		pr.CloseWithError(err)
		authed <- err
	}()
	src := &errReader{r: rc}
	_, copyErr := io.Copy(io.MultiWriter(tmp, pw), src)
	pw.CloseWithError(copyErr)
	authErr := <-authed
	if cerr := tmp.Close(); copyErr == nil {
		copyErr = cerr
	}
	switch {
	case src.err != nil:
		// The download broke; the opener only saw it secondhand.
		return src.err
	case authErr != nil:
		return fmt.Errorf("the object in the source bucket does not authenticate: %w", authErr)
	}
	return copyErr
}

// errReader remembers the first error other than EOF its reader returned.
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

// Retire stops writes to a target; it stays readable and listed.
func (m *Manager) Retire(name string) error {
	if _, err := m.Store.OffsiteTargetByName(name); err != nil {
		return err
	}
	return m.Store.SetOffsiteTargetRole(name, "retired")
}

// Activate brings a retired target back into the nightly loop.
func (m *Manager) Activate(name string) error {
	if _, err := m.Store.OffsiteTargetByName(name); err != nil {
		return err
	}
	return m.Store.SetOffsiteTargetRole(name, "active")
}
