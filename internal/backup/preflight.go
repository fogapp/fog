// Fog's half of the fog-backup/1 preflight (docs/backup-contract.md): the
// checks that can be made from the artifact and fog's own records, run before
// anything is staged or armed.
//
// The app re-runs the full preflight at restore time and has the final word —
// it alone knows its migration state and its database's extensions. What fog
// adds is timing. Staging arms a marker the app consumes on its next boot, so
// an artifact fog accepts blindly becomes an app that boot-loops on a restore
// it can never complete. Refusing here turns that into an error on the request
// the operator actually made.
package backup

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/vault"
)

// Manifest is the part of manifest.json fog reads. The contract allows
// additive fields, so unknown ones are ignored rather than rejected.
type Manifest struct {
	Format     string `json:"format"`
	App        string `json:"app"`
	CreatedAt  string `json:"created_at"`
	ProducedBy struct {
		GitSHA        string `json:"git_sha"`
		SchemaVersion string `json:"schema_version"`
	} `json:"produced_by"`
	Components []ManifestComponent `json:"components"`
	Identity   struct {
		Host         string `json:"host"`
		WebauthnRPID string `json:"webauthn_rp_id"`
	} `json:"identity"`
	// SecretsFingerprint maps env key -> "sha256:" + first 12 hex of the
	// value's digest. Never the value.
	SecretsFingerprint map[string]string `json:"secrets_fingerprint"`
	Excluded           []string          `json:"excluded"`
}

type ManifestComponent struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// PreflightResult follows the contract's verdict shape: errors refuse the
// restore, warnings are things the operator has already confirmed (an
// unconfirmed identity mismatch is an error, not a warning).
type PreflightResult struct {
	Manifest *Manifest `json:"manifest,omitempty"`
	Errors   []string  `json:"errors"`
	Warnings []string  `json:"warnings"`
}

func (r *PreflightResult) OK() bool { return len(r.Errors) == 0 }

// Err joins the refusals into one error, so a caller can hand the whole
// verdict to the operator instead of only the first thing that was wrong.
func (r *PreflightResult) Err() error {
	if r.OK() {
		return nil
	}
	return errors.New(strings.Join(r.Errors, "; "))
}

// fingerprint is the contract's secrets_fingerprint form.
func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// InspectArtifact reads an artifact's manifest without hashing its contents —
// the cheap read behind the install flow's preview. Verification is
// PreflightArtifact's job.
func InspectArtifact(artifactPath string) (*Manifest, error) {
	raw, err := readManifest(artifactPath)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if !KnownArtifactFormat(m.Format) {
		return nil, fmt.Errorf("artifact declares format %q, expected %q or %q", m.Format, ArtifactFormat, ArtifactFormatV2)
	}
	return &m, nil
}

// PreflightArtifact runs the checks fog can make on its own: format and app
// identity, component checksums, the secrets fingerprints that decide whether
// restored ciphertext will be readable, and the host-identity confirmation.
//
// identity is what fog knows the app's software to be — the `app` its
// descriptor or its earlier artifacts declare ("hub", "feed") — and "" when
// fog has no way to know yet (restore-at-create). It is deliberately not the
// app's name in fog: that is an instance label ("hub-lxc"), and an artifact
// from any hub belongs in any hub.
//
// Schema version and Postgres extensions are deliberately absent. Both depend
// on the code and database the app is about to boot with, which fog cannot see
// before the image is built — the app checks them, and refusing to guess here
// is better than a verdict that looks authoritative and isn't.
//
// secretFor returns the value fog holds for an env key, or "" when it holds
// none. It is called only for keys the artifact fingerprints, so nothing else
// is decrypted.
//
// forgetSecrets is the operator's decision that the secrets the artifact was
// written with are gone for good — a server rebuilt without its recovery
// kit. A missing or different secret is then a warning, not a refusal: the
// app restores and clears whatever those secrets encrypted, since no key
// left anywhere can read it. Every other check still refuses.
func PreflightArtifact(artifactPath, appName, identity string, secretFor func(key string) string, confirmIdentity, forgetSecrets bool) *PreflightResult {
	res := &PreflightResult{Errors: []string{}, Warnings: []string{}}
	fail := func(format string, args ...any) *PreflightResult {
		res.Errors = append(res.Errors, fmt.Sprintf(format, args...))
		return res
	}

	if _, err := os.Stat(artifactPath); err != nil {
		return fail("no artifact at %s", artifactPath)
	}
	raw, digests, err := scanArtifact(artifactPath)
	if err != nil {
		return fail("artifact is not readable as a tar: %s", err)
	}
	if raw == nil {
		return fail("artifact has no manifest.json")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fail("manifest is not valid JSON: %s", err)
	}
	res.Manifest = &m

	if !KnownArtifactFormat(m.Format) {
		fail("artifact declares format %q, expected %q or %q", m.Format, ArtifactFormat, ArtifactFormatV2)
	}
	if msg, warn := checkIdentity(m.App, identity, appName); msg != "" {
		fail("%s", msg)
	} else if warn != "" {
		res.Warnings = append(res.Warnings, warn)
	}

	for _, c := range m.Components {
		got, ok := digests[c.Path]
		if !ok {
			fail("component %s is listed in the manifest but missing from the artifact", c.Path)
			continue
		}
		if !strings.EqualFold(got.sha256, c.SHA256) {
			fail("component %s fails checksum verification — the artifact is corrupt", c.Path)
		} else if got.bytes != c.Bytes {
			fail("component %s is %d bytes, manifest says %d", c.Path, got.bytes, c.Bytes)
		}
	}

	// The check that saves a migration: the artifact's data was encrypted
	// with these values, so a mismatch means rows that decrypt to nothing.
	// It is also the one an operator can still fix at this point.
	for key, want := range m.SecretsFingerprint {
		have := ""
		if secretFor != nil {
			have = secretFor(key)
		}
		switch {
		case have == "" && forgetSecrets:
			res.Warnings = append(res.Warnings, key+" is not set here (restoring without it: the app clears what it encrypted)")
		case have == "":
			fail("%s is not set here but the artifact's data was written with it — restoring would produce unreadable ciphertext", key)
		case strings.EqualFold(fingerprint(have), want):
		case forgetSecrets:
			res.Warnings = append(res.Warnings, key+" differs from the one the artifact was written with (restoring without it: the app clears what it encrypted)")
		default:
			fail("%s differs from the one the artifact was written with — restoring would produce unreadable ciphertext; set it to the source instance's value first", key)
		}
	}

	if m.Identity.Host != "" {
		msg := fmt.Sprintf("artifact was produced for host %q", m.Identity.Host)
		if confirmIdentity {
			res.Warnings = append(res.Warnings, msg+" (confirmed, proceeding)")
		} else {
			fail("%s; confirm the identity change to proceed anyway", msg)
		}
	}
	return res
}

// SecretReader supplies PreflightArtifact with the values fog holds for an
// existing app: ciphertext from the store, decrypted through the vault, one
// key at a time so nothing beyond the artifact's fingerprinted keys is ever
// decrypted. Anything it cannot produce reads as "" — which preflight reports
// as a refusal, the safe reading of "fog does not have this value".
//
// Plain spec env is the fallback. Today's delegated apps fingerprint only
// vault secrets, but an app is free to fingerprint any restore-critical env
// value, and refusing a restore over a value fog does in fact hold — just not
// in the vault — would be a false refusal in a gate the operator cannot argue
// with.
func (m *Manager) SecretReader(appName string) func(key string) string {
	var appID int64
	env := map[string]string{}
	if app, err := m.Store.AppByName(appName); err == nil {
		appID = app.ID
		if spec, err := apps.LatestSpec(m.Store, app.ID); err == nil && spec != nil {
			env = spec.Env
		}
	}
	return func(key string) string {
		if appID != 0 && m.Engine != nil && m.Engine.Vault != nil {
			secrets := vault.Secrets{Store: m.Store, Vault: m.Engine.Vault}
			if value, err := secrets.Get(appID, key); err == nil && value != "" {
				return value
			}
		}
		return env[key]
	}
}

type entryDigest struct {
	sha256 string
	bytes  int64
}

// Bounds on what an artifact tar may hold. A fog-backup/1 artifact is a
// manifest, a dump and a handful of component archives, so these are far past
// any real one — and they keep a hostile one (endless entries, or PAX names
// megabytes long) from exhausting memory in the duplicate check.
const (
	maxArtifactEntries = 10000
	maxEntryNameBytes  = 4096
)

// entryGuard admits an artifact's entries one by one: it refuses one past the
// entry limit, a name past the length limit, and a name already seen (after
// cleaning — "./x" and "x" are one name). Names are remembered by their
// SHA-256, so memory is bounded by the entry count alone.
type entryGuard struct {
	seen map[[32]byte]struct{}
	n    int
}

func newEntryGuard() *entryGuard { return &entryGuard{seen: map[[32]byte]struct{}{}} }

// count admits one header (directories included) against the entry limit.
func (g *entryGuard) count() error {
	g.n++
	if g.n > maxArtifactEntries {
		return fmt.Errorf("artifact has more than %d entries — refusing it", maxArtifactEntries)
	}
	return nil
}

// name admits one cleaned entry name, refusing an over-long or repeated one.
func (g *entryGuard) name(name string) error {
	if len(name) > maxEntryNameBytes {
		return fmt.Errorf("artifact has an entry name %d bytes long, past the %d-byte limit — refusing it", len(name), maxEntryNameBytes)
	}
	k := sha256.Sum256([]byte(name))
	if _, dup := g.seen[k]; dup {
		return fmt.Errorf("artifact has more than one entry named %q — refusing an ambiguous artifact", name)
	}
	g.seen[k] = struct{}{}
	return nil
}

// scanArtifact makes one pass over the tar, pulling manifest.json and hashing
// every other entry. Artifacts are GB-scale, so checksum verification must
// never need them extracted.
func scanArtifact(artifactPath string) ([]byte, map[string]entryDigest, error) {
	f, err := os.Open(artifactPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return scanTar(f)
}

// scanTar is scanArtifact over an artifact already open, so a caller can
// hash the whole stream in the same pass.
// SparseEntry says a tar entry is stored sparse (GNU or PAX sparse): its
// holes expand on reading, so a few kilobytes of artifact become gigabytes
// on disk. Fog never needs one, and refuses it.
func SparseEntry(hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// errSparse refuses a sparse entry, naming it.
func errSparse(name string) error {
	return fmt.Errorf("artifact entry %q is stored sparse, which fog does not accept", name)
}

func scanTar(r io.Reader) ([]byte, map[string]entryDigest, error) {
	var manifest []byte
	digests := map[string]entryDigest{}
	guard := newEntryGuard()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return manifest, digests, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if err := guard.count(); err != nil {
			return nil, nil, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if SparseEntry(hdr) {
			return nil, nil, errSparse(hdr.Name)
		}
		name := strings.TrimPrefix(filepath.Clean(hdr.Name), "./")
		// Two entries that name the same thing (a literal duplicate, or an
		// ambiguity like "./x" vs "x" or a trailing slash, all of which clean
		// to one name) make "which copy is authoritative" undecidable: the
		// checksum pass would read one and a restore the other. Refuse it.
		if err := guard.name(name); err != nil {
			return nil, nil, err
		}
		if name == "manifest.json" {
			manifest, err = io.ReadAll(io.LimitReader(tr, manifestLimit))
			if err != nil {
				return nil, nil, err
			}
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, tr)
		if err != nil {
			return nil, nil, err
		}
		digests[name] = entryDigest{sha256: hex.EncodeToString(h.Sum(nil)), bytes: n}
	}
}

// checkIdentity compares the software an artifact declares against the
// software fog knows the app to be. Fog's own name for the app never enters
// into it — "hub-lxc" is a label; the artifact's "hub" and the descriptor's
// "hub" are the identity. With no known identity the app's own preflight has
// the final word, and fog says so rather than guessing.
func checkIdentity(artifactApp, identity, appName string) (fail, warn string) {
	switch {
	case artifactApp == "":
		return "artifact declares no app", ""
	case identity == "":
		return "", fmt.Sprintf("the artifact is for %q; fog cannot confirm before %s boots that this is the same software — the app's own preflight refuses a mismatch", artifactApp, appName)
	case !apps.SameSoftware(artifactApp, identity):
		return fmt.Sprintf("artifact is for %q but %s runs %q", artifactApp, appName, identity), ""
	}
	return "", ""
}
