package build

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
)

// The attack: one app's repo writes into a RUN --mount=type=cache
// directory, and another app's build then reads it — the shared builder
// keys a cache mount by its target path alone. With a builder per app, the
// second build finds its cache empty.
//
// Needs docker with buildx (a Linux Docker host; on a Mac with Colima, run
// it inside the VM); skipped without them.
func TestLiveBuildCacheDoesNotCrossApps(t *testing.T) {
	if testing.Short() || exec.Command("docker", "buildx", "version").Run() != nil {
		t.Skip("needs docker buildx")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	sfx := strings.ToLower(randx.Hex(3))
	poisoner, victim := "poisoner"+sfx, "victim"+sfx
	const base = "FROM alpine:3.20\n"
	poison := base + "RUN --mount=type=cache,target=/cache sh -c 'echo poisoned > /cache/marker'\n"
	read := base + "RUN --mount=type=cache,target=/cache sh -c 'cat /cache/marker > /result 2>/dev/null || echo clean > /result'\n"

	b := &Builder{DataDir: t.TempDir()}
	write := func(app, dockerfile string) {
		dir := filepath.Join(b.appDir(app), "repo")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644)
	}
	result := func(img string) string {
		out, _ := exec.CommandContext(ctx, "docker", "run", "--rm", img, "cat", "/result").CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		for _, a := range []string{poisoner, victim} {
			b.RemoveBuilder(context.Background(), a)
		}
		exec.Command("docker", "rmi", "-f", "ctl-p"+sfx, "ctl-v"+sfx, imageRef(poisoner, "0123456789ab"), imageRef(victim, "0123456789ab")).Run()
	})

	// Control: on the shared builder, the victim reads the poison.
	ctl := t.TempDir()
	os.WriteFile(filepath.Join(ctl, "P"), []byte(poison), 0o644)
	os.WriteFile(filepath.Join(ctl, "V"), []byte(read), 0o644)
	for _, c := range [][]string{{"-f", filepath.Join(ctl, "P"), "-t", "ctl-p" + sfx}, {"-f", filepath.Join(ctl, "V"), "-t", "ctl-v" + sfx}} {
		if out, err := exec.CommandContext(ctx, "docker", append(append([]string{"build", "-q"}, c...), ctl)...).CombinedOutput(); err != nil {
			t.Skipf("docker build: %s", out)
		}
	}
	if got := result("ctl-v" + sfx); got != "poisoned" {
		t.Skipf("control: the shared builder didn't share the cache (%q); the test proves nothing here", got)
	}
	exec.CommandContext(ctx, "docker", "builder", "prune", "-f", "--filter", "type=exec.cachemount").Run()

	write(poisoner, poison)
	write(victim, read)
	sha := "0123456789abcdef0123456789abcdef01234567"
	if _, err := b.Build(ctx, poisoner, sha, "", nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	img, err := b.Build(ctx, victim, sha, "", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := result(img); got != "clean" {
		t.Fatalf("the victim's build read %q from another app's cache", got)
	}
}
