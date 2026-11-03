package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SignedManifest answers the release manifest fog's update source serves
// now and its signature, exactly as signed, once the signature verifies
// against the pinned release keys. fogd hands both to the machines that
// lend it their hardware (internal/compute): a machine installs a fog-node
// build only when the manifest verifies against the key the machine pins
// itself and names the build's digest, so fog's word for a build is never
// what decides it.
func (u *Updater) SignedManifest(ctx context.Context) (body, sig []byte, err error) {
	s, err := parseSource(u.baseURL())
	if err != nil {
		return nil, nil, err
	}
	keys, err := u.releaseKeys()
	if err != nil {
		return nil, nil, err
	}
	var errs []string
	var stale *StaleError
	for _, c := range s.candidates(u.branch()) {
		body, err := u.getBytes(ctx, c.Manifest, maxManifestBytes)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		sig, err := u.getBytes(ctx, c.Manifest+".sig", 4096)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: unsigned: %v", c.Manifest, err))
			continue
		}
		if err := verifySignature(body, sig, keys); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", c.Manifest, err))
			continue
		}
		// The same age rule as every other path (MaxManifestAge): a machine
		// is never handed an old release, however validly signed, and the
		// fog-node build that receives it checks again.
		if err := checkIssued(body, c.Manifest, u.now()); err != nil {
			errs = append(errs, err.Error())
			var se *StaleError
			if errors.As(err, &se) && (stale == nil || se.Issued.After(stale.Issued)) {
				stale = se
			}
			continue
		}
		return body, sig, nil
	}
	if len(errs) == 0 {
		return nil, nil, errors.New("no release manifest found")
	}
	if stale != nil {
		return nil, nil, fmt.Errorf("no current signed release manifest found (%s): %w", strings.Join(errs, "; "), stale)
	}
	return nil, nil, fmt.Errorf("no signed release manifest found (%s)", strings.Join(errs, "; "))
}

// IsNewer reports whether candidate is an upgrade from current, by the
// order self-update uses: a fog-node takes only a strictly newer release,
// so a replayed older one, however validly signed, can't roll it back.
func IsNewer(candidate, current string) bool { return isNewer(candidate, current) }
