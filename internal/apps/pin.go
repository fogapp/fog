package apps

// Pinned bind mounts (docs/files-contract.md, "Mounts are pinned").
//
// A bind source is a path, and Docker resolves it again at every container
// start — the restart policy's and the daemon's at boot included, before
// fogd runs. A Drive folder or host path whose parent an app can write (a
// downloader with Content beside a library with Content/Movies; one app with
// Apps/X and Apps/X/y) could be swapped for a symbolic link to / between
// starts, and the next start would bind the host's root read-write.
//
// So fog resolves each such folder itself, once, with no link followed,
// from a descriptor, and bind-mounts that folder at
// <data>/binds/<app>/<volume> — a path only fogd writes — and compose binds
// that. A link swapped in afterwards changes nothing: the pin is the folder,
// not the path. <data>/binds is a tmpfs fogd mounts, so after a reboot the
// pins are simply absent until fogd makes them again, compose says the
// source must exist (create_host_path: false), and Docker refuses to start
// the container instead of binding anything else. fogd then pins and starts
// what should be running (ReapplyRules).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BindsDirName is the folder under fog's data directory that holds pins.
const BindsDirName = "binds"

// PinFunc pins rel under root at target, re-owning it to owner if set.
type PinFunc func(ctx context.Context, root, rel, target, owner string) error

// UnpinFunc removes the pins at target: a pin, or an app's folder of them
// but for those named in keep.
type UnpinFunc func(ctx context.Context, target string, keep ...string) error

// BindsDir is where the app's pins live.
func (m *Materializer) BindsDir() string { return filepath.Join(m.DataDir, BindsDirName) }

// PinPath is where one volume of app is pinned.
func (m *Materializer) PinPath(app, volume string) string {
	return filepath.Join(m.BindsDir(), app, volume)
}

// pinSource is what a volume pins: the trusted root and the path below it.
// A Drive folder resolves under Drive's root; a host path under /. A
// managed volume needs no pin — nothing an app writes is above it.
func pinSource(v Volume, filesRoot string) (root, rel string, ok bool) {
	switch {
	case v.Files != "" && filesRoot != "":
		return filesRoot, filepath.FromSlash(v.Files), true
	case v.Host != "":
		return "/", strings.TrimPrefix(filepath.Clean(v.Host), "/"), true
	}
	return "", "", false
}

// checkPinTarget refuses a target outside binds — the helper runs as root.
func checkPinTarget(binds, target string) error {
	b, t := filepath.Clean(binds), filepath.Clean(target)
	if !strings.HasPrefix(t, b+string(filepath.Separator)) {
		return fmt.Errorf("%s is not under %s", target, binds)
	}
	return nil
}

func parseOwner(owner string) (int, int, error) {
	u, g, ok := strings.Cut(owner, ":")
	uid, err1 := strconv.Atoi(u)
	gid, err2 := strconv.Atoi(g)
	if !ok || err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("owner %q is not uid:gid", owner)
	}
	return uid, gid, nil
}

// PinMain is `fogd pin-bind`: the helper fogd runs in the host's mount
// namespace (fogd's own is private, and a mount made there would be
// invisible to Docker), as root. Returns the exit code.
func PinMain(args []string) int {
	fs := flag.NewFlagSet("pin-bind", flag.ContinueOnError)
	binds := fs.String("binds", "", "fog's binds directory")
	root := fs.String("root", "", "trusted root the folder is under")
	rel := fs.String("rel", "", "the folder, relative to root")
	target := fs.String("target", "", "where to pin it, under binds")
	owner := fs.String("owner", "", "uid:gid to give the folder's root")
	unpin := fs.Bool("unpin", false, "remove the pin(s) at target")
	keep := fs.String("keep", "", "with --unpin on an app's folder: comma-separated pins to keep")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *binds == "" || *target == "" || (!*unpin && *root == "") {
		fmt.Fprintln(os.Stderr, "pin-bind: --binds, --target and (to pin) --root are required")
		return 2
	}
	var err error
	if *unpin {
		var k []string
		if *keep != "" {
			k = strings.Split(*keep, ",")
		}
		err = unpinBind(*binds, *target, k...)
	} else {
		err = pinBind(*binds, *root, *rel, *target, *owner)
	}
	var w *PinWarning
	switch {
	case errors.As(err, &w):
		fmt.Fprintln(os.Stderr, "pin-bind: warning:", w.Msg)
		return PinExitWarning
	case errors.Is(err, iofs.ErrNotExist):
		// Said apart, so the caller can tell storage not there yet from a
		// refusal (a link, a folder that isn't one).
		fmt.Fprintln(os.Stderr, "pin-bind:", err)
		return PinExitNotFound
	case err != nil:
		fmt.Fprintln(os.Stderr, "pin-bind:", err)
		return 1
	}
	return 0
}

// pin-bind's exit codes beyond 0, 1 (refused) and 2 (usage): a pin that
// took with a warning, and a source that isn't there (yet).
const (
	PinExitWarning  = 3
	PinExitNotFound = 4
)

// PinArgs are the `fogd pin-bind` arguments for one pin.
func PinArgs(binds, root, rel, target, owner string) []string {
	args := []string{"pin-bind", "--binds", binds, "--root", root, "--rel", rel, "--target", target}
	if owner != "" {
		args = append(args, "--owner", owner)
	}
	return args
}

// UnpinArgs are the `fogd pin-bind` arguments for one unpin.
func UnpinArgs(binds, target string, keep []string) []string {
	args := []string{"pin-bind", "--unpin", "--binds", binds, "--target", target}
	if len(keep) > 0 {
		args = append(args, "--keep", strings.Join(keep, ","))
	}
	return args
}

// checkNestedVolumes refuses, where fog can't pin, a writable volume of the
// app that holds another of its volumes: the app could replace the inner
// folder with a link between starts, and the next start would bind
// whatever the link names. Pinned, the inner volume is its folder and the
// swap changes nothing, so this is only asked when pinning is unavailable.
func checkNestedVolumes(s *AppSpec, filesRoot string) error {
	vols := s.AllVolumes()
	for i, outer := range vols {
		if outer.ReadOnly {
			continue
		}
		op := sharedHostPath(outer, filesRoot)
		if op == "" {
			continue
		}
		for j, inner := range vols {
			if i == j {
				continue
			}
			ip := sharedHostPath(inner, filesRoot)
			if ip != "" && ip != op && within(ip, op) {
				return fmt.Errorf("volume %s is inside volume %s, which the app can write: it could swap %s for a link to anywhere. Mount one or the other", inner.Name, outer.Name, inner.Name)
			}
		}
	}
	return nil
}
