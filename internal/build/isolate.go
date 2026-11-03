package build

// Build cache isolation. A Dockerfile's RUN --mount=type=cache mounts a
// directory the builder keeps between builds — shared by every build on the
// builder, keyed by nothing more than its target path. A malicious repo
// could fill /root/.npm with what another app's build then installed. So
// each app builds on a BuildKit builder of its own (docker buildx, the
// docker-container driver), whose cache is that builder's alone.
//
// Each builder also sits on a network of its own (fog-build-<app>), not
// Docker's default bridge: a RUN step keeps BuildKit's default capabilities,
// NET_RAW and MKNOD among them — BuildKit has no setting that drops them —
// so on a shared bridge one app's Dockerfile could ARP-spoof, or simply
// reach, another app's builder mid-build. And each RUN step gets a network
// namespace of its own behind a bridge inside the builder (BuildKit's
// bridge worker network), rather than the builder's own: what a RUN step
// can forge stays on that inner bridge, whose only other members are the
// same app's steps.
//
// Where that can't be had (no buildx plugin, or its builder image can't be
// fetched), fog falls back to the shared builder — one build at a time, and
// every cache mount emptied before each — so a cache still can't carry
// anything from one app's build to another's, only build more slowly.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"gitea.lan/matthew/fog/internal/dockerx"
	"gitea.lan/matthew/fog/internal/execx"
)

// BuilderName is the buildx builder an app's builds run on.
func BuilderName(app string) string { return "fog-build-" + app }

// sharedMu serialises builds on the shared builder in the fallback.
var sharedMu sync.Mutex

// isolate answers the docker arguments that start an isolated build of app
// ("buildx build --builder … --load", or "build" on the fallback) and what
// to call when the build is done.
func (b *Builder) isolate(ctx context.Context, app string, log io.Writer) ([]string, func()) {
	ensure := b.ensureBuilder
	if ensure == nil {
		ensure = ensureBuilder
	}
	err := ensure(ctx, app)
	if err == nil {
		return []string{"buildx", "build", "--builder", BuilderName(app), "--load"}, func() {}
	}
	fmt.Fprintf(log, "note: no builder of %s's own (%v); building on the shared one, its cache emptied first\n", app, err)
	sharedMu.Lock()
	pruneCacheMounts(ctx)
	return []string{"build"}, func() {
		pruneCacheMounts(context.WithoutCancel(ctx))
		sharedMu.Unlock()
	}
}

// builderLabel marks a builder's network as fog's, naming its app.
const builderLabel = "fog.build"

// builderFlags are buildkitd's: every RUN step in a namespace of its own on
// the builder's inner bridge.
const builderFlags = "--oci-worker-net bridge"

// ensureBuilder makes app's builder if it isn't there, on its own network.
// One made before builders had networks of their own — on Docker's default
// bridge, every RUN step in the builder's own namespace — is made again.
func ensureBuilder(ctx context.Context, app string) error {
	name := BuilderName(app)
	if out, err := execx.Output(ctx, "docker", "buildx", "inspect", name); err == nil {
		if builderCurrent(out, name) {
			return nil
		}
		if _, err := execx.Output(ctx, "docker", "buildx", "rm", "--force", name); err != nil {
			return fmt.Errorf("replacing %s, made on the shared network: %w", name, err)
		}
	}
	// On a subnet from fog's pool (dockerx), not Docker's default ones: a
	// network per git app would soon use those up.
	if _, err := execx.Output(ctx, "docker", "network", "inspect", name); err != nil {
		if _, err := dockerx.New().EnsureIsolatedNetwork(ctx, name, false, map[string]string{builderLabel: app}); err != nil {
			return fmt.Errorf("the builder's own network: %w", err)
		}
	}
	_, err := execx.Output(ctx, "docker", "buildx", "create", "--name", name,
		"--driver", "docker-container", "--driver-opt", "network="+name,
		"--buildkitd-flags", builderFlags, "--bootstrap")
	return err
}

// builderCurrent reports whether buildx's description of builder name says
// it is on its own network with every RUN step on the inner bridge.
func builderCurrent(inspect, name string) bool {
	return strings.Contains(inspect, `network="`+name+`"`) && strings.Contains(inspect, builderFlags)
}

// pruneCacheMounts empties every cache mount on the shared builder.
func pruneCacheMounts(ctx context.Context) {
	execx.Output(ctx, "docker", "builder", "prune", "--force", "--filter", "type=exec.cachemount")
}

// RemoveBuilder deletes app's builder, its cache and its network, with the
// app.
func (b *Builder) RemoveBuilder(ctx context.Context, app string) {
	execx.Output(ctx, "docker", "buildx", "rm", "--force", BuilderName(app))
	execx.Output(ctx, "docker", "network", "rm", BuilderName(app))
}
