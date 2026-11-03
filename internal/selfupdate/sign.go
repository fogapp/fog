package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// A release manifest is signed: release.sh writes latest.json.sig beside
// latest.json, an ed25519 signature over the manifest's exact bytes, made
// with a key only fog's release process holds. fogd refuses a manifest whose
// signature doesn't verify against a key pinned here, and every artifact is
// then checked against the manifest's sha256 — so a mirror, a proxy, or
// anyone on the path to a plain-http source can withhold an update but can
// never hand this server a binary fog didn't release.
//
// ReleaseKeys are the public halves, base64 of the raw 32 bytes. More than
// one is how a key rotates: a release signed by the old key pins the new one
// beside it, and a later release drops the old. install.sh and
// imaging/flash.sh pin the same keys (as SPKI PEM); shellsource_test.go holds
// the copies together. A fork that releases its own builds replaces this
// list with its own key (go run ./internal/selfupdate/signtool keygen).
var ReleaseKeys = []string{
	"zrGQX7hwCDQn0KNY31d+VHb3CU2wIg/qt7CeHpDzMUg=", // fog release key, 2026-11
}

// EnvAllowUnsigned turns signature checking off. It exists for a host that
// builds its own unsigned releases, and is set on the unit by hand — the
// settings page can't reach it, so a compromised account can't either.
const EnvAllowUnsigned = "FOG_UPDATE_ALLOW_UNSIGNED"

const maxManifestBytes = 1 << 20

// MaxManifestAge is how old a signed manifest may be and still be used —
// install.sh, the Proxmox scripts and flash.sh hold the same 180 days
// (MAX_MANIFEST_AGE). Signatures never expire on their own, so without it a
// mirror could serve an old release, validly signed, to a fresh install
// forever, or freeze a running server on it. fog's release process signs a
// fresh manifest with every release; a project quiet for longer re-signs
// (re-runs release.sh) to stay installable.
const MaxManifestAge = 180 * 24 * time.Hour

const staleNoticeKind = "selfupdate-stale"

// StaleError is a manifest that verified but is older than MaxManifestAge
// (or carries no issue date).
type StaleError struct {
	URL    string
	Issued time.Time // zero when undated
}

func (e *StaleError) Error() string {
	if e.Issued.IsZero() {
		return e.URL + ": undated (made before releases carried issued_at) — refusing it"
	}
	return fmt.Sprintf("%s: issued %s, more than %d days ago — refusing it", e.URL, e.Issued.UTC().Format("2006-01-02"), int(MaxManifestAge.Hours()/24))
}

func (e *StaleError) notice(source string) string {
	when := "with no issue date"
	if !e.Issued.IsZero() {
		when = "issued " + e.Issued.UTC().Format("2 January 2006")
	}
	return fmt.Sprintf("The newest release %s offers is signed, but %s — older than the %d days fog accepts. "+
		"Either the project has gone quiet, or something between this server and its source (a stale mirror, a proxy) "+
		"is holding back newer releases. Fog keeps running as it is; it just won't update from this source.\n\n"+
		"Check that %s is where fog's releases are published now (Settings › Server › Updates), and compare it with the project's release page.",
		source, when, int(MaxManifestAge.Hours()/24), source)
}

var errNoKeys = errors.New("no release keys are pinned in this build")

func pinnedKeys() ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, s := range ReleaseKeys {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("pinned release key %q is malformed", s)
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	if len(keys) == 0 {
		return nil, errNoKeys
	}
	return keys, nil
}

// VerifyReleaseManifest is how anything that will act on a manifest checks
// it: the signature against the pinned release keys, then the signed
// issued_at against MaxManifestAge. fog-node checks the manifest fogd hands
// it this way before installing a build (internal/computenode).
func VerifyReleaseManifest(body, sig []byte) error {
	if err := VerifyManifestSignature(body, sig); err != nil {
		return err
	}
	return checkIssued(body, "the release manifest", time.Now())
}

// checkIssued refuses a manifest whose signed issued_at is missing or older
// than MaxManifestAge.
func checkIssued(body []byte, where string, now time.Time) error {
	var m struct {
		IssuedAt int64 `json:"issued_at"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("%s: not a release manifest", where)
	}
	if m.IssuedAt <= 0 {
		return &StaleError{URL: where}
	}
	if issued := time.Unix(m.IssuedAt, 0); now.Sub(issued) > MaxManifestAge {
		return &StaleError{URL: where, Issued: issued}
	}
	return nil
}

// VerifyManifestSignature checks a latest.json.sig (base64, surrounding
// whitespace ignored) over body against the pinned release keys — the
// signature only. A manifest something will act on goes through
// VerifyReleaseManifest, which also enforces its age.
func VerifyManifestSignature(body, sig []byte) error {
	keys, err := pinnedKeys()
	if err != nil {
		return err
	}
	return verifySignature(body, sig, keys)
}

func verifySignature(body, sig []byte, keys []ed25519.PublicKey) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("the manifest's signature is malformed")
	}
	for _, k := range keys {
		if ed25519.Verify(k, body, raw) {
			return nil
		}
	}
	return errors.New("the manifest's signature doesn't match fog's release key")
}

// releaseKeys is the key set this updater checks against: a test's own, or
// the pinned ones.
func (u *Updater) releaseKeys() ([]ed25519.PublicKey, error) {
	if u.keys != nil {
		return u.keys, nil
	}
	return pinnedKeys()
}

func allowUnsigned() bool { return os.Getenv(EnvAllowUnsigned) == "1" }
