package fsx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/execx"
)

// probeTimeout bounds the child. A share that has stopped answering hangs
// the write, and the caller is a background sweep, not a person waiting.
const probeTimeout = 15 * time.Second

// CanWriteAs answers whether uid:gid can create a file in dir — the only
// question that matters about a folder fog is about to hand an app, and the
// one question fogd cannot answer by trying it itself. fogd is root: on an
// NFS export that squashes every identity but root, or a share whose ACL
// names accounts the uid isn't one of, root writes happily and the chown to
// files_owner succeeds while conferring nothing. Only a write as that uid
// tells the truth, so this forks one.
//
// ok is the verdict; detail carries the child's own words for it ("Permission
// denied"). A non-nil err means the probe could not run at all — no verdict,
// which callers must not read as a failure.
func CanWriteAs(ctx context.Context, dir string, uid, gid int) (ok bool, detail string, err error) {
	if dir == "" {
		return false, "", errors.New("no directory to probe")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// The child creates and removes exactly one uniquely named dotfile of
	// its own: a probe that runs on every sweep must never touch, and never
	// leave behind, anything the operator put there.
	const script = `f="$1/.fog-owner-probe.$$"; touch "$f" || exit 1; rm -f "$f"`
	cmd := execx.Command(ctx, "/bin/sh", "-c", script, "sh", dir)
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true,
	}
	out, runErr := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if runErr == nil {
		return true, "", nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) && ee.ExitCode() == 1 {
		// The write itself was refused — that is a verdict, whatever the
		// reason the kernel gave for it.
		if msg == "" {
			msg = "the write was refused"
		}
		return false, lastLine(msg), nil
	}
	if msg != "" {
		return false, "", fmt.Errorf("%w: %s", runErr, lastLine(msg))
	}
	return false, "", runErr
}

// ParseOwner reads "uid:gid".
func ParseOwner(v string) (uid, gid int, ok bool) {
	a, b, found := strings.Cut(strings.TrimSpace(v), ":")
	if !found {
		return 0, 0, false
	}
	uid, err1 := strconv.Atoi(a)
	gid, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || uid < 0 || gid < 0 {
		return 0, 0, false
	}
	return uid, gid, true
}

// lastLine keeps the part of a shell's complaint that names the reason,
// dropping the "sh: line 1:" preamble some shells prepend.
func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}
