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

	"gitea.lan/matthew/fog/internal/livetest"
	"gitea.lan/matthew/fog/internal/randx"
)

// The attack: a RUN step keeps NET_RAW (BuildKit has no way to drop it),
// and every builder sat on Docker's default bridge — so one app's
// Dockerfile could reach, or ARP-spoof, another app's builder mid-build,
// and anything else on that bridge. Each builder is now on a network of its
// own, and each RUN step in a namespace of its own behind the builder: a
// probe from one app's build reaches neither the other app's builder nor a
// container on the default bridge. A builder made the old way is replaced.
//
// Needs docker with buildx (inside the Colima VM on a Mac).
func TestLiveBuildersCannotReachEachOther(t *testing.T) {
	if testing.Short() || exec.Command("docker", "buildx", "version").Run() != nil {
		t.Skip("needs docker buildx")
	}
	livetest.Lock(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	sfx := strings.ToLower(randx.Hex(3))
	attacker, victim, bystander := "attacker"+sfx, "victim"+sfx, "fog-bystander-"+sfx
	b := &Builder{DataDir: t.TempDir()}
	t.Cleanup(func() {
		for _, a := range []string{attacker, victim} {
			b.RemoveBuilder(context.Background(), a)
		}
		exec.Command("docker", "rm", "-f", bystander).Run()
		exec.Command("docker", "rmi", "-f", imageRef(attacker, "0123456789ab"), "fog-probe-ctl-"+sfx).Run()
	})
	if out, err := docker("run", "-d", "--name", bystander, "alpine:3.20", "sleep", "600"); err != nil {
		t.Skipf("bystander: %s", out)
	}
	ip := func(container string) string {
		out, err := docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", container)
		if err != nil || strings.TrimSpace(out) == "" {
			t.Fatalf("address of %s: %s", container, out)
		}
		return strings.Fields(out)[0]
	}
	bystanderIP := ip(bystander)

	// The attacker's builder, made the old way: on the default bridge.
	if out, err := docker("buildx", "create", "--name", BuilderName(attacker), "--driver", "docker-container", "--bootstrap"); err != nil {
		t.Skipf("buildx create: %s", out)
	}
	probe := func(targets ...string) string {
		var sb strings.Builder
		sb.WriteString("FROM alpine:3.20\nRUN grep CapEff /proc/self/status > /result")
		for _, ip := range targets {
			sb.WriteString(" && (ping -c 2 -W 1 " + ip + " >/dev/null 2>&1 && echo REACHED " + ip + " || echo BLOCKED " + ip + ") >> /result")
		}
		return sb.String() + "\n"
	}
	result := func(img string) string {
		out, _ := docker("run", "--rm", img, "cat", "/result")
		return out
	}
	ctl := t.TempDir()
	os.WriteFile(filepath.Join(ctl, "Dockerfile"), []byte(probe(bystanderIP)), 0o644)
	if out, err := docker("buildx", "build", "--builder", BuilderName(attacker), "--load", "--no-cache", "-t", "fog-probe-ctl-"+sfx, ctl); err != nil {
		t.Fatalf("control build: %s", out)
	}
	if got := result("fog-probe-ctl-" + sfx); !strings.Contains(got, "REACHED "+bystanderIP) {
		t.Skipf("control: the old builder couldn't reach the default bridge (%q); the test proves nothing here", got)
	}

	// The victim's builder, made now; then the attacker's build, which
	// replaces its old builder first.
	if err := ensureBuilder(ctx, victim); err != nil {
		t.Fatal(err)
	}
	victimIP := ip("buildx_buildkit_" + BuilderName(victim) + "0")
	dir := filepath.Join(b.appDir(attacker), "repo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(probe(victimIP, bystanderIP)), 0o644)
	img, err := b.Build(ctx, attacker, "0123456789abcdef0123456789abcdef01234567", "", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := result(img)
	for _, target := range []string{victimIP, bystanderIP} {
		if !strings.Contains(got, "BLOCKED "+target) {
			t.Errorf("the attacker's build reached %s:\n%s", target, got)
		}
	}
	if out, _ := docker("buildx", "inspect", BuilderName(attacker)); !builderCurrent(out, BuilderName(attacker)) {
		t.Errorf("the old builder wasn't replaced:\n%s", out)
	}
	if out, _ := docker("inspect", "-f", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", "buildx_buildkit_"+BuilderName(attacker)+"0"); strings.TrimSpace(out) != BuilderName(attacker) {
		t.Errorf("the attacker's builder is on %q", out)
	}
	t.Logf("RUN step: %s", got)
}
