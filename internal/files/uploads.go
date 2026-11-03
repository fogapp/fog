package files

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/jobs"
)

// Uploads are sessions, not requests. A session names the destination and
// the size up front, then takes bytes in chunks at a declared offset until
// it is complete, at which point the file is verified and renamed into
// place. Chunks keep every request short — the Cloudflare tunnel caps
// bodies at 100 MB and browsers time out long ones — and a dropped
// connection costs one chunk, not the file. The part file and its receipt
// live under <root>/.fog/uploads so the final rename never crosses a
// filesystem — except that an upload into Apps or Content while that folder
// lives on another disk stages its part in the folder's own .fog/uploads
// (Stage), for the same reason.

const uploadsDir = ReservedDir + "/uploads"

// uploadMaxAge bounds how long an unfinished upload part may sit; a
// closed laptop lid should not leak disk forever.
const uploadMaxAge = 24 * time.Hour

// Upload is a session as the API sees it.
type Upload struct {
	ID     string `json:"id"`
	Path   string `json:"path"` // destination, root-relative
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	// Skipped is set when the conflict policy was skip and the name is
	// taken: no session was created.
	Skipped bool `json:"skipped,omitempty"`
}

// uploadMeta is the on-disk receipt beside the part file.
type uploadMeta struct {
	ID         string   `json:"id"`
	Path       string   `json:"path"`
	Size       int64    `json:"size"`
	SHA256     string   `json:"sha256,omitempty"` // what the client claims, if it does
	Mtime      string   `json:"mtime,omitempty"`  // RFC3339, the source file's own
	OnConflict Conflict `json:"onConflict"`
	CreatedAt  string   `json:"createdAt"`
	// Digest state so the hash rides along with the chunks: Hashed is how
	// many bytes HashState covers. If it ever falls behind the part file
	// (a crash between the write and the receipt), completion rehashes.
	Hashed    int64  `json:"hashed"`
	HashState []byte `json:"hashState,omitempty"`
	// Owned: an owning subsystem's upload (a peer's contribution landing
	// at its origin); the write guards do not apply, through completion.
	Owned bool `json:"owned,omitempty"`
	// Stage is the placed folder whose own reserved directory holds the
	// part file; empty for the root's.
	Stage string `json:"stage,omitempty"`
}

// ErrOffset is returned with the real offset when a chunk arrives at the
// wrong place; the client resynchronizes from it.
type ErrOffset struct{ Offset int64 }

func (e *ErrOffset) Error() string {
	return fmt.Sprintf("upload offset mismatch: the session is at %d", e.Offset)
}

var ErrUploadIncomplete = errors.New("the upload is not complete")

// ErrNoSpace refuses an upload bigger than the room left where it would
// land.
var ErrNoSpace = errors.New("not enough free space")

// uploadLocks serializes the requests of one session: two chunks sent at
// the same offset (a client retrying while the first is still in flight)
// would both pass the offset check and append, interleaved, to one part.
var uploadLocks sync.Map // id -> *sync.Mutex

func lockUpload(id string) func() {
	mu, _ := uploadLocks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// endUpload is lockUpload for the last request of a session (complete,
// abort): the lock is dropped from the map with it, so finished sessions
// do not accumulate.
func endUpload(id string) func() {
	unlock := lockUpload(id)
	return func() {
		uploadLocks.Delete(id)
		unlock()
	}
}

var ErrUploadCorrupt = errors.New("the uploaded bytes do not match the declared digest")

func uploadReceipt(id string) string { return uploadsDir + "/" + id + ".json" }

// part is where the session's bytes accumulate.
func (u uploadMeta) part() string { return stagedUploads(u.Stage) + "/" + u.ID + ".part" }

func stagedUploads(stage string) string {
	if stage != "" && isPlaceable(stage) {
		return stagingDir(stage) + "/uploads"
	}
	return uploadsDir
}

func validUploadID(id string) bool {
	if len(id) != 16 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (m *Manager) readUpload(root *os.Root, id string) (uploadMeta, error) {
	if !validUploadID(id) {
		return uploadMeta{}, ErrNotFound
	}
	b, err := nf(root).ReadFile(uploadReceipt(id))
	if err != nil {
		return uploadMeta{}, ErrNotFound
	}
	var u uploadMeta
	if err := json.Unmarshal(b, &u); err != nil || u.ID != id {
		return uploadMeta{}, ErrNotFound
	}
	return u, nil
}

func (m *Manager) writeUpload(root *os.Root, u uploadMeta) error {
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	tmp := uploadReceipt(u.ID) + ".tmp"
	if err := nf(root).WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return nf(root).Rename(tmp, uploadReceipt(u.ID))
}

func partSize(root *os.Root, u uploadMeta) (int64, error) {
	info, err := lstatNF(root, u.part())
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%w: the upload's part file was replaced", ErrNotFound)
	}
	return info.Size(), nil
}

// BeginUpload creates a session. The destination's parent directories are
// created (a folder dropped on the page arrives as files with relative
// paths). A taken name follows the conflict policy: ask refuses now, skip
// answers Skipped, rename and replace are applied at completion.
func (m *Manager) BeginUpload(path string, size int64, sha, mtime string, c Conflict) (Upload, error) {
	return m.beginUpload(path, size, sha, mtime, c, false)
}

// BeginUploadOwned is BeginUpload for an owning subsystem: the write
// guards are not consulted, at begin or at completion (a replace may
// trash a guarded file — a peer superseding its own contribution).
func (m *Manager) BeginUploadOwned(path string, size int64, sha, mtime string, c Conflict) (Upload, error) {
	return m.beginUpload(path, size, sha, mtime, c, true)
}

func (m *Manager) beginUpload(path string, size int64, sha, mtime string, c Conflict, owned bool) (Upload, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return Upload{}, err
	}
	if path == "" {
		return Upload{}, ErrBadName
	}
	if size < 0 {
		return Upload{}, fmt.Errorf("%w: negative size", ErrBadName)
	}
	parent, name := Split(path)
	if err := ValidateName(parent, name); err != nil {
		return Upload{}, err
	}
	if !owned {
		if err := m.guardWrite(path); err != nil {
			return Upload{}, err
		}
	}
	if sha != "" {
		if len(sha) != 64 {
			return Upload{}, fmt.Errorf("%w: sha256 must be 64 hex characters", ErrBadName)
		}
		sha = strings.ToLower(sha)
	}
	// The declared size has to fit where the part is staged, which is the
	// destination's own disk: a session that cannot finish is refused now,
	// not after the disk has filled under everything else on it.
	if avail, _, ok := diskSpace(filepath.Join(m.Root(), filepath.FromSlash(m.placedFolder(path)))); ok && size > avail {
		return Upload{}, fmt.Errorf("%w: %s needs %d bytes and %d are free", ErrNoSpace, path, size, avail)
	}
	if parent != "" {
		if err := mkdirAllNF(root, parent, 0o755); err != nil {
			return Upload{}, err
		}
		m.chownNewDirs(root, parent)
	}
	if exists(root, path) {
		switch c {
		case ConflictSkip:
			return Upload{Path: path, Size: size, Skipped: true}, nil
		case ConflictAsk:
			m.refreshDir(root, ix, parent, false)
			n, _ := ix.Get(path)
			return Upload{}, &ConflictError{Path: path, Existing: n}
		}
	}
	u := uploadMeta{ID: jobs.NewID(), Path: path, Size: size, SHA256: sha, Mtime: mtime, OnConflict: c, CreatedAt: time.Now().UTC().Format(time.RFC3339), Owned: owned, Stage: m.placedFolder(path)}
	for _, dir := range []string{uploadsDir, stagedUploads(u.Stage)} {
		if err := mkdirAllNF(root, dir, 0o700); err != nil {
			return Upload{}, mapOSError(err)
		}
	}
	f, err := nf(root).OpenFile(u.part(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Upload{}, mapOSError(err)
	}
	f.Close()
	if err := m.writeUpload(root, u); err != nil {
		nf(root).Remove(u.part())
		return Upload{}, err
	}
	return Upload{ID: u.ID, Path: path, Size: size}, nil
}

// chownNewDirs applies files_owner up the chain of a just-created parent.
func (m *Manager) chownNewDirs(root *os.Root, rel string) {
	for rel != "" {
		m.chown(root, rel)
		rel, _ = Split(rel)
	}
}

// UploadStatus reports where a session stands.
func (m *Manager) UploadStatus(id string) (Upload, error) {
	root, err := m.openRoot()
	if err != nil {
		return Upload{}, err
	}
	u, err := m.readUpload(root, id)
	if err != nil {
		return Upload{}, err
	}
	off, err := partSize(root, u)
	if err != nil {
		return Upload{}, err
	}
	return Upload{ID: u.ID, Path: u.Path, Size: u.Size, Offset: off}, nil
}

// AppendUpload writes one chunk at offset. The part file is the source of
// truth for the offset; a mismatch returns ErrOffset with the real one.
func (m *Manager) AppendUpload(id string, offset int64, body io.Reader) (Upload, error) {
	root, err := m.openRoot()
	if err != nil {
		return Upload{}, err
	}
	if !validUploadID(id) {
		return Upload{}, ErrNotFound
	}
	defer lockUpload(id)()
	u, err := m.readUpload(root, id)
	if err != nil {
		return Upload{}, err
	}
	cur, err := partSize(root, u)
	if err != nil {
		return Upload{}, err
	}
	if offset != cur {
		return Upload{ID: u.ID, Path: u.Path, Size: u.Size, Offset: cur}, &ErrOffset{Offset: cur}
	}
	f, err := openNF(root, u.part(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Upload{}, err
	}
	// Hash alongside the write when the digest state is current.
	var h hash.Hash
	if u.Hashed == cur {
		h = sha256.New()
		if len(u.HashState) > 0 {
			if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(u.HashState); err != nil {
				h = nil
			}
		}
	}
	var w io.Writer = f
	if h != nil {
		w = io.MultiWriter(f, h)
	}
	// Never past the declared size: a client that lies about the size
	// gets its file cut off at what it declared, and told so.
	n, err := io.Copy(w, io.LimitReader(body, u.Size-cur+1))
	if err != nil {
		f.Close()
		return Upload{}, err
	}
	if cur+n > u.Size {
		f.Close()
		nf(root).Remove(u.part())
		nf(root).Remove(uploadReceipt(id))
		return Upload{}, fmt.Errorf("%w: more bytes than the declared size", ErrBadName)
	}
	if err := f.Close(); err != nil {
		return Upload{}, err
	}
	if h != nil {
		u.HashState, _ = h.(encoding.BinaryMarshaler).MarshalBinary()
		u.Hashed = cur + n
	}
	if err := m.writeUpload(root, u); err != nil {
		return Upload{}, err
	}
	return Upload{ID: u.ID, Path: u.Path, Size: u.Size, Offset: cur + n}, nil
}

// CompleteUpload verifies the part and moves it into place.
func (m *Manager) CompleteUpload(id string) (Node, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return Node{}, err
	}
	if !validUploadID(id) {
		return Node{}, ErrNotFound
	}
	defer endUpload(id)()
	u, err := m.readUpload(root, id)
	if err != nil {
		return Node{}, err
	}
	cur, err := partSize(root, u)
	if err != nil {
		return Node{}, err
	}
	if cur != u.Size {
		return Node{}, fmt.Errorf("%w: %d of %d bytes received", ErrUploadIncomplete, cur, u.Size)
	}
	// The digest: from the chunk-side state when it kept up, else a full read.
	var sum string
	if u.Hashed == u.Size && len(u.HashState) > 0 {
		h := sha256.New()
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(u.HashState); err == nil {
			sum = hex.EncodeToString(h.Sum(nil))
		}
	}
	if sum == "" {
		f, _, err := openFileNF(root, u.part())
		if err != nil {
			return Node{}, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return Node{}, err
		}
		sum = hex.EncodeToString(h.Sum(nil))
	}
	if u.SHA256 != "" && sum != u.SHA256 {
		nf(root).Remove(u.part())
		nf(root).Remove(uploadReceipt(id))
		return Node{}, ErrUploadCorrupt
	}
	// Durable before visible; the mode is set through the descriptor, so it
	// lands on the part and nothing else.
	if f, err := openNF(root, u.part(), os.O_RDWR, 0); err == nil {
		f.Sync()
		f.Chmod(0o644)
		f.Close()
	}
	if u.Mtime != "" {
		if t, err := time.Parse(time.RFC3339, u.Mtime); err == nil {
			nf(root).Chtimes(u.part(), t, t)
		}
	}
	m.chown(root, u.part())

	parent, name := Split(u.Path)
	dest, err := m.resolveDestAs(root, ix, parent, name, u.OnConflict, u.Owned, nil)
	if err != nil {
		return Node{}, err
	}
	if dest == "" {
		// Skip: the name was taken between begin and complete.
		nf(root).Remove(u.part())
		nf(root).Remove(uploadReceipt(id))
		return Node{}, &ConflictError{Path: u.Path}
	}
	if err := nf(root).RenameNoReplace(u.part(), dest); err != nil {
		return Node{}, mapOSError(err)
	}
	nf(root).Remove(uploadReceipt(id))
	if _, err := m.refreshDir(root, ix, parent, false); err != nil {
		return Node{}, err
	}
	n, err := ix.Get(dest)
	if err != nil {
		return Node{}, err
	}
	// The digest is known; record it so the hasher need not read the file
	// again and duplicates show at once.
	ix.SetHash(dest, n.Size, n.mtime, sum)
	n.SHA256 = sum
	return n, nil
}

// AbortUpload discards a session.
func (m *Manager) AbortUpload(id string) error {
	root, err := m.openRoot()
	if err != nil {
		return err
	}
	if !validUploadID(id) {
		return ErrNotFound
	}
	defer endUpload(id)()
	u, err := m.readUpload(root, id)
	if err != nil {
		return err
	}
	nf(root).Remove(u.part())
	return nf(root).Remove(uploadReceipt(id))
}

// sweepUploads removes parts and receipts nothing has touched for a day,
// in the root's staging and in each placed folder's.
func (m *Manager) sweepUploads(root *os.Root) {
	m.sweepUploadsIn(root, uploadsDir)
	for _, folder := range placeable {
		if m.placedFolder(folder) != "" {
			m.sweepUploadsIn(root, stagedUploads(folder))
		}
	}
}

func (m *Manager) sweepUploadsIn(root *os.Root, dir string) {
	dfd, err := openDirFD(root, dir)
	if err != nil {
		return
	}
	// Each entry lstat'ed through the folder's descriptor (readEntriesAt):
	// a path made from a descriptor's name is not a path from the working
	// directory, and stat'ing one failed for every entry, so nothing was
	// ever swept.
	ents, err := readEntriesAt(dfd)
	unix.Close(dfd)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-uploadMaxAge)
	for _, info := range ents {
		e := info
		// A file moved aside is someone's file, not an upload: it is put
		// back (restoreAside), never swept.
		if strings.HasSuffix(e.Name(), ".was") || strings.HasSuffix(e.Name(), ".was.json") {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := nf(root).RemoveAll(dir + "/" + e.Name()); err == nil {
			m.Log.Info("removed abandoned upload", "name", e.Name(), "bytes", info.Size())
		}
	}
}
