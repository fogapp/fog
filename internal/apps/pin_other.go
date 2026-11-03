//go:build !linux

package apps

import "errors"

// pinSupported: pinning needs Linux mounts; elsewhere (macOS development)
// fog binds paths as they are and refuses what that would make unsafe
// within one app (checkNestedVolumes).
const pinSupported = false

var errNoPin = errors.New("pinned bind mounts need Linux")

func pinBind(binds, root, rel, target, owner string) error { return errNoPin }
func unpinBind(binds, target string, keep ...string) error { return errNoPin }
