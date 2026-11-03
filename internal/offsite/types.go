package offsite

import "time"

// The sealed JSON documents inside a locker (docs/locker-contract.md §3).
// Each is small, versioned by Format, and additive: readers ignore
// unknown keys.

// AppRow describes one app backup — the store's backups row, denormalized
// so the locker is self-describing without fog's database.
type AppRow struct {
	Format string `json:"format"`
	// App is fog's name for the installation; Identity is the software
	// (the fog-backup/1 manifest's app), when known.
	App      string `json:"app"`
	Identity string `json:"identity,omitempty"`
	Stamp    string `json:"stamp"`
	Kind     string `json:"kind"` // manual | scheduled | pre-update | pre-rollback
	// Mode is "delegated" (artifact.tar + manifest.json) or "volumes"
	// (volume tars + dumps + meta.json).
	Mode      string `json:"mode"`
	CreatedAt string `json:"created_at"`
	// SizeBytes is the snapshot directory's size; SnapshotSHA256 is the
	// digest of the plaintext tar the .snapshot object seals.
	SizeBytes      int64  `json:"size_bytes"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	// Delegated only: the artifact's own format, digest and manifest.
	ArtifactFormat string `json:"artifact_format,omitempty"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	Manifest       string `json:"manifest,omitempty"`
	VerifiedAt     string `json:"verified_at,omitempty"`
	// Files lists the snapshot directory's entries, so a reader knows
	// what to expect before unpacking.
	Files []string `json:"files"`
	// SourceBackupID is the row id on the fog that wrote it — a hint for
	// that same fog, meaningless elsewhere.
	SourceBackupID int64 `json:"source_backup_id,omitempty"`
}

// FilesSnapshot is one Files-tree snapshot: the local .fog-snapshot.json
// plus, per file, the blob that holds its content.
type FilesSnapshot struct {
	Format    string      `json:"format"`
	Stamp     string      `json:"stamp"`
	CreatedAt string      `json:"created_at"`
	Root      string      `json:"root"`
	Files     []FileEntry `json:"files"`
	Excluded  []string    `json:"excluded,omitempty"`
	Bytes     int64       `json:"bytes"`
	Count     int64       `json:"count"`
	// SourceSnapshotID is the file_snapshots row id on the writing fog.
	SourceSnapshotID int64 `json:"source_snapshot_id,omitempty"`
}

// FileEntry is one file in a Files snapshot.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"` // unix nanos
	SHA256 string `json:"sha256"`
	Blob   string `json:"blob"`
}

// Index is the rollup written last on every successful run: one GET
// answers "what is in this locker" for a restore wizard, on providers
// that charge for listing as much as on those that don't. A cache of the
// truth, never the truth — readers fall back to listing when it is
// missing or older than the newest object.
type Index struct {
	Format    string     `json:"format"`
	LockerID  string     `json:"locker_id"`
	Fog       string     `json:"fog"`
	WrittenAt string     `json:"written_at"`
	Kits      []IndexKit `json:"kits"`
	// LatestKit is the newest kit this fog uploaded itself, with the
	// digest of its sealed bytes: what a recovery should find, sealed under
	// the locker's keys so a bucket writer can neither forge nor alter it.
	LatestKit *IndexKit           `json:"latest_kit,omitempty"`
	Apps      map[string][]AppRow `json:"apps"`
	Files     []IndexFiles        `json:"files"`
}

// IndexKit is a recovery kit in the locker, stored as-is from disk.
type IndexKit struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Stamp string `json:"stamp"`
	// SHA256 is the digest of the kit's sealed bytes, set for a kit a fog
	// recorded uploading (its own, or carried from an earlier index); a kit
	// only seen in the bucket listing has none.
	SHA256 string `json:"sha256,omitempty"`
	// Received is when the bucket says it received the object (listing
	// only): a kit is never named for a time after it reached the bucket.
	Received time.Time `json:"-"`
}

// IndexFiles summarizes one Files snapshot.
type IndexFiles struct {
	Stamp     string `json:"stamp"`
	CreatedAt string `json:"created_at"`
	Files     int64  `json:"files"`
	Bytes     int64  `json:"bytes"`
}
