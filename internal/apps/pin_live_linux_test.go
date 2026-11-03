//go:build linux

package apps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"gitea.lan/matthew/fog/internal/randx"
)

// The reviewer's attack, on a real Linux Docker host: a folder some other
// path's bind source runs through is swapped for a link to /, and a
// container start — `docker restart`, or the restart policy — then binds
// the host's root read-write. With pins it must bind the pinned folder, or
// not start; never / or /etc.
//
// Needs Linux, root (it mounts) and docker. Run it on the Docker host; on a
// Mac with Colima:
//
//	GOOS=linux GOARCH=arm64 go test -c -o ~/apps.test ./internal/apps
//	colima ssh -- sudo ~/apps.test -test.run LivePinned -test.v
func TestLivePinnedBindsSurviveLinkSwaps(t *testing.T) {
	if testing.Short() || os.Geteuid() != 0 {
		t.Skip("live: needs root on a Docker host")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := docker("image", "inspect", liveNetImage); err != nil {
		if out, err := docker("pull", liveNetImage); err != nil {
			t.Skipf("cannot pull %s: %s", liveNetImage, out)
		}
	}
	base, err := os.MkdirTemp("/var/tmp", "fogpin")
	if err != nil {
		t.Fatal(err)
	}
	drive := filepath.Join(base, "drive")
	binds := filepath.Join(base, "binds")
	t.Cleanup(func() {
		unpinBind(binds, filepath.Join(binds, "stream"))
		unpinBind(binds, filepath.Join(binds, "evil"))
		unix.Unmount(binds, unix.MNT_DETACH)
		os.RemoveAll(base)
	})
	for _, d := range []string{"Content/Movies", "Apps/Evil/x"} {
		if err := os.MkdirAll(filepath.Join(drive, d), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(drive, "Content/Movies/movie.mkv"), []byte("m"), 0o644)
	os.WriteFile(filepath.Join(drive, "Apps/Evil/x/x-file"), []byte("x"), 0o644)
	sfx := strings.ToLower(randx.Hex(3))
	run := func(name string, args ...string) string {
		t.Helper()
		id, err := docker(append([]string{"run", "-d", "--name", name + sfx}, args...)...)
		if err != nil {
			t.Fatalf("docker run %s: %s", name, id)
		}
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
		return id
	}
	hostRoot := func(listing string) bool {
		f := strings.Fields(listing)
		has := func(n string) bool {
			for _, x := range f {
				if x == n {
					return true
				}
			}
			return false
		}
		return has("etc") && (has("usr") || has("proc"))
	}

	// Across apps: Stream binds Content/Movies; qBittorrent writes Content.
	movies := filepath.Join(binds, "stream", "movies")
	if err := pinBind(binds, drive, "Content/Movies", movies, ""); err != nil {
		t.Fatal(err)
	}
	stream := run("stream", "--restart", "unless-stopped",
		"--mount", "type=bind,source="+movies+",target=/data", liveNetImage, "sleep", "3600")
	// The swap, keeping the folder: moved aside, a link in its place.
	if out, err := docker("run", "--rm", "-v", filepath.Join(drive, "Content")+":/data", liveNetImage,
		"sh", "-c", "mv /data/Movies /data/Movies.old && ln -s / /data/Movies"); err != nil {
		t.Fatalf("the swap: %s", out)
	}
	// The attack is real: a raw path bind now lands on the host's root.
	if out, _ := docker("run", "--rm", "-v", filepath.Join(drive, "Content/Movies")+":/m", liveNetImage, "ls", "/m"); !hostRoot(out) {
		t.Fatalf("control: the raw path didn't follow the link (%q); the test proves nothing", out)
	}
	if out, err := docker("restart", stream); err != nil {
		t.Fatalf("restart: %s", out)
	}
	if out, err := docker("exec", stream, "ls", "/data"); err != nil || hostRoot(out) || !strings.Contains(out, "movie.mkv") {
		t.Fatalf("after a restart the pinned bind lists %q (%v), want the pinned folder", out, err)
	}
	// The swap, deleting the folder: the pin holds a folder that is gone,
	// and the start fails rather than bind anything else.
	if out, err := docker("run", "--rm", "-v", filepath.Join(drive, "Content")+":/data", liveNetImage,
		"sh", "-c", "rm -rf /data/Movies.old"); err != nil {
		t.Fatalf("the delete: %s", out)
	}
	if _, err := docker("restart", stream); err == nil {
		if out, _ := docker("exec", stream, "ls", "/data"); hostRoot(out) {
			t.Fatalf("after the folder was deleted, the restart bound %q", out)
		}
	}
	// Pinning again refuses the link; the pin in place stays the folder.
	if err := pinBind(binds, drive, "Content/Movies", movies, ""); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("re-pin through the link: %v", err)
	}
	if out, _ := docker("exec", stream, "ls", "/data"); hostRoot(out) {
		t.Fatalf("after a refused re-pin: %q", out)
	}

	// One app, nested volumes, its own restart policy: the first run plants
	// the link and exits, the restart binds the inner volume again.
	outer, inner := filepath.Join(binds, "evil", "a"), filepath.Join(binds, "evil", "b")
	if err := pinBind(binds, drive, "Apps/Evil", outer, ""); err != nil {
		t.Fatal(err)
	}
	if err := pinBind(binds, drive, "Apps/Evil/x", inner, ""); err != nil {
		t.Fatal(err)
	}
	evil := run("evil", "--restart", "unless-stopped",
		"--mount", "type=bind,source="+outer+",target=/a", "--mount", "type=bind,source="+inner+",target=/b",
		liveNetImage, "sh", "-c", "if [ -L /a/x ]; then echo SECOND; ls /b; sleep 3600; else mv /a/x /a/x.old; ln -s / /a/x; exit 1; fi")
	var logs string
	for range 60 {
		logs, _ = docker("logs", evil)
		if strings.Contains(logs, "SECOND") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(logs, "SECOND") {
		t.Fatalf("the restart policy never restarted it: %q", logs)
	}
	if hostRoot(logs) || !strings.Contains(logs, "x-file") {
		t.Fatalf("the restarted container bound %q, want its pinned inner folder", logs)
	}

	// Boot: the tmpfs is gone, so the pins are. Docker must refuse to start
	// a container made while they were there, not bind an empty folder or
	// anything else.
	booted, err := docker("create", "--name", "booted"+sfx, "--mount", "type=bind,source="+outer+",target=/data", liveNetImage, "ls", "/data")
	if err != nil {
		t.Fatalf("create: %s", booted)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", booted).Run() })
	if err := unix.Unmount(binds, unix.MNT_DETACH); err != nil {
		t.Fatal(err)
	}
	if out, err := docker("start", "-a", booted); err == nil {
		t.Fatalf("started with its pin gone, binding %q", out)
	}
}
