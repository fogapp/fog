package apps

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"sonarr", "my-app", "a", "app2"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-app", "app-", "App", "a b", "caddy", "fog", strings.Repeat("a", 40)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestSpecValidate(t *testing.T) {
	spec := &AppSpec{
		Source:  Source{Type: "image", Image: "lscr.io/linuxserver/sonarr:latest"},
		Env:     map[string]string{"PUID": "1000"},
		Volumes: []Volume{{Name: "config", Mount: "/config"}},
		Routes:  []Route{{Port: 8989}},
		Health:  &Health{Path: "/ping"},
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	bad := &AppSpec{Source: Source{Type: "image"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("empty image accepted")
	}
	badEnv := &AppSpec{Source: Source{Type: "image", Image: "x"}, Env: map[string]string{"BAD KEY": "v"}}
	if err := badEnv.Validate(); err == nil {
		t.Fatal("bad env key accepted")
	}
}

func TestRouteHost(t *testing.T) {
	for _, tc := range []struct{ name, host, domain, want string }{
		{"no host uses the app name", "", "example.com", "openclaw.example.com"},
		{"bare label is a subdomain", "claw", "example.com", "claw.example.com"},
		{"dotted host is verbatim", "claw.other.net", "example.com", "claw.other.net"},
		{"a label needs a domain to resolve", "claw", "", ""},
		{"no domain, no hostname", "", "", ""},
	} {
		if got := RouteHost("openclaw", Route{Host: tc.host, Port: 80}, tc.domain); got != tc.want {
			t.Errorf("%s: RouteHost(host=%q, domain=%q) = %q, want %q", tc.name, tc.host, tc.domain, got, tc.want)
		}
	}
}

func TestRouteHostValidate(t *testing.T) {
	base := func(routes ...Route) *AppSpec {
		return &AppSpec{Source: Source{Type: "image", Image: "x"}, Routes: routes}
	}
	for _, host := range []string{"claw", "claw.example.com", "a1-b2.example.co.uk"} {
		if err := base(Route{Host: host, Port: 80}).Validate(); err != nil {
			t.Errorf("valid host %q rejected: %v", host, err)
		}
	}
	for name, spec := range map[string]*AppSpec{
		"scheme":       base(Route{Host: "https://claw.example.com", Port: 80}),
		"path":         base(Route{Host: "example.com/claw", Port: 80}),
		"space":        base(Route{Host: "my claw", Port: 80}),
		"uppercase":    base(Route{Host: "Claw", Port: 80}),
		"leading dash": base(Route{Host: "-claw", Port: 80}),
		"empty label":  base(Route{Host: "claw..example.com", Port: 80}),
		"duplicate":    base(Route{Host: "claw", Port: 80}, Route{Host: "claw", Port: 81}),
	} {
		if err := spec.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPortsValidate(t *testing.T) {
	base := func(ports []Port, strategy string) *AppSpec {
		return &AppSpec{
			Source: Source{Type: "image", Image: "adguard/adguardhome:latest"},
			Routes: []Route{{Port: 3000}}, Health: &Health{Path: "/"},
			Ports: ports, DeployStrategy: strategy,
		}
	}
	// Same port on both protocols is legal — that's exactly the DNS shape.
	ok := base([]Port{{Port: 53, Protocol: "tcp"}, {Port: 53, Protocol: "udp"}}, "restart")
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid ports rejected: %v", err)
	}
	for name, bad := range map[string]*AppSpec{
		"port out of range": base([]Port{{Port: 70000}}, "restart"),
		"bad protocol":      base([]Port{{Port: 53, Protocol: "sctp"}}, "restart"),
		"duplicate host port": base([]Port{
			{Port: 53, Protocol: "udp"}, {Port: 5353, HostPort: 53, Protocol: "udp"},
		}, "restart"),
		"ports with swap": base([]Port{{Port: 53}}, "swap"),
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDirectPort(t *testing.T) {
	for name, tc := range map[string]struct {
		spec AppSpec
		want bool
	}{
		"no routes":        {AppSpec{}, false},
		"ordinary":         {AppSpec{Routes: []Route{{Port: 80}}}, true},
		"gated":            {AppSpec{Routes: []Route{{Port: 80, Auth: true}}}, false},
		"lan only":         {AppSpec{Routes: []Route{{Port: 80, LanOnly: true}}}, false},
		"later route only": {AppSpec{Routes: []Route{{Port: 80}, {Host: "b", Port: 81, LanOnly: true}}}, true},
	} {
		if got := tc.spec.DirectPort(); got != tc.want {
			t.Errorf("%s = %v", name, got)
		}
	}
}

func TestValidateNetwork(t *testing.T) {
	spec := func(s AppSpec) *AppSpec {
		s.Source = Source{Type: "image", Image: "x:1"}
		return &s
	}
	for name, s := range map[string]*AppSpec{
		"default":    spec(AppSpec{}),
		"sealed":     spec(AppSpec{Network: NetworkSealed}),
		"peer":       spec(AppSpec{SealedPeer: true}),
		"peer ports": spec(AppSpec{SealedPeer: true, Ports: []Port{{Port: 53}}}),
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	for name, s := range map[string]*AppSpec{
		"unknown network": spec(AppSpec{Network: "private"}),
		"sealed peer":     spec(AppSpec{Network: NetworkSealed, SealedPeer: true}),
		"sealed ports":    spec(AppSpec{Network: NetworkSealed, Ports: []Port{{Port: 53}}}),
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// renderedCompose renders spec as "app" and decodes what compose would read.
func renderedCompose(t *testing.T, spec *AppSpec, gateway string) (services map[string]map[string]any, networks map[string]any) {
	t.Helper()
	m := &Materializer{DataDir: t.TempDir()}
	if err := m.Render("app", spec, map[string]string{"K": "v"}, "x:1", gateway, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.AppDir("app"), "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
		Networks map[string]any            `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Services, doc.Networks
}

func joined(service map[string]any) []string {
	var out []string
	for n := range service["networks"].(map[string]any) {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Every container a sealed app runs has only networks without a route out:
// its workers, and its sidecars, whose compose network would otherwise be the
// default route for the whole app.
func TestRenderSealed(t *testing.T) {
	services, networks := renderedCompose(t, &AppSpec{
		Network:   NetworkSealed,
		Processes: []Process{{Name: "worker", Command: []string{"work"}}},
		Aux:       []AuxService{{Name: "redis", Image: "redis:7"}},
	}, "172.30.0.1")

	own := sealedNetPrefix + "app"
	for _, shared := range []string{NetworkName, SealedNetworkName, appNetPrefix + "app"} {
		if _, ok := networks[shared]; ok {
			t.Errorf("a sealed app declares %s: %v", shared, networks)
		}
	}
	if got := networks[own].(map[string]any)["external"]; got != true {
		t.Errorf("%s is fog's to create, not compose's: %v", own, networks)
	}
	// fog makes the sidecar network (internal, for a sealed app) on a subnet
	// of its own: compose only names it.
	if p := networks["private"].(map[string]any); p["external"] != true || p["name"] != sealedAuxNetPrefix+"app" {
		t.Errorf("the sidecar network isn't fog's sealed one: %v", networks)
	}
	if got := joined(services["app"]); !slices.Equal(got, []string{own, "private"}) {
		t.Errorf("main joined %v", got)
	}
	if got := joined(services["app-worker"]); !slices.Equal(got, []string{own, "private"}) {
		t.Errorf("worker joined %v", got)
	}
	if got := joined(services["redis"]); !slices.Equal(got, []string{"private"}) {
		t.Errorf("sidecar joined %v", got)
	}
	// The proxy finds it by name on its own network, and fogd keeps the name
	// apps call it by, on an address it can actually reach.
	main := services["app"]["networks"].(map[string]any)[own].(map[string]any)
	if fmt.Sprint(main["aliases"]) != "[app app.fog-app]" {
		t.Errorf("no alias on %s: %v", own, main)
	}
	for _, s := range []string{"app", "app-worker"} {
		if got := fmt.Sprint(services[s]["extra_hosts"]); got != "[host.docker.internal:172.30.0.1]" {
			t.Errorf("%s extra_hosts = %s", s, got)
		}
	}
}

// A sealed peer is an ordinary app on its own network: sealed apps reach it
// over a wire (netwire.go), not over a network every sealed app shares.
func TestRenderSealedPeer(t *testing.T) {
	services, networks := renderedCompose(t, &AppSpec{SealedPeer: true, Processes: []Process{{Name: "worker", Command: []string{"work"}}}, Aux: []AuxService{{Name: "redis", Image: "redis:7"}}}, "172.30.0.1")
	own := appNetPrefix + "app"
	if got := networks[own].(map[string]any)["external"]; got != true {
		t.Errorf("%s isn't external: %v", own, networks)
	}
	for _, shared := range []string{NetworkName, SealedNetworkName} {
		if _, ok := networks[shared]; ok {
			t.Errorf("a peer declares the shared %s: %v", shared, networks)
		}
	}
	if got := joined(services["app-worker"]); !slices.Equal(got, []string{own, "private"}) {
		t.Errorf("worker joined %v", got)
	}
	// A peer keeps its way out, and its sidecars stay its own.
	if got := fmt.Sprint(services["app"]["extra_hosts"]); got != "[host.docker.internal:host-gateway]" {
		t.Errorf("peer extra_hosts = %s", got)
	}
	if p := networks["private"].(map[string]any); p["external"] != true || p["name"] != auxNetPrefix+"app" {
		t.Errorf("a peer's sidecar network isn't its own ordinary one: %v", networks)
	}
	if got := joined(services["redis"]); !slices.Equal(got, []string{"private"}) {
		t.Errorf("sidecar joined %v", got)
	}
}

// An ordinary app is on its own network alone: never the shared bridge
// every other app used to be on.
func TestRenderOrdinaryAppStaysOnItsOwnNetwork(t *testing.T) {
	services, networks := renderedCompose(t, &AppSpec{}, "")
	if len(networks) != 1 || networks[appNetPrefix+"app"] == nil {
		t.Errorf("networks = %v", networks)
	}
	if got := joined(services["app"]); !slices.Equal(got, []string{appNetPrefix + "app"}) {
		t.Errorf("joined %v", got)
	}
}

// A sealed app with no address for fogd would deploy and then fail to report
// anything, quietly.
func TestRenderSealedNeedsTheGateway(t *testing.T) {
	m := &Materializer{DataDir: t.TempDir()}
	if err := m.Render("app", &AppSpec{Network: NetworkSealed}, nil, "x:1", "", io.Discard); err == nil {
		t.Fatal("rendered a sealed app with no gateway")
	}
}

func TestRender(t *testing.T) {
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	spec := &AppSpec{
		Source:                Source{Type: "image", Image: "lscr.io/linuxserver/sonarr:latest"},
		Volumes:               []Volume{{Name: "config", Mount: "/config"}, {Name: "media", Mount: "/data", Host: "/mnt/media"}},
		AcknowledgeHostAccess: []string{"/mnt/media"},
		Files:                 []File{{Name: "extra.conf", Mount: "/etc/extra.conf", Content: "key=value\n"}},
	}
	env := map[string]string{"PUID": "1000", "SECRET_TOKEN": `va"lue with spaces`}
	if err := m.Render("sonarr", spec, env, spec.Source.Image, "", io.Discard); err != nil {
		t.Fatal(err)
	}

	compose, err := os.ReadFile(filepath.Join(dir, "apps", "sonarr", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c := string(compose)
	for _, want := range []string{
		"image: lscr.io/linuxserver/sonarr:latest",
		"/mnt/media:/data",
		filepath.Join(dir, "data", "sonarr", "config") + ":/config",
		"extra.conf:/etc/extra.conf:ro",
		"fog.app: sonarr",
		"external: true",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose.yaml missing %q\n%s", want, c)
		}
	}

	envFile, err := os.ReadFile(filepath.Join(dir, "apps", "sonarr", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	// Double-quoted with everything compose acts on escaped. See
	// escapeEnvValue and TestEnvFileRoundTripFuzz.
	if !strings.Contains(string(envFile), "PUID=\"1000\"\n") {
		t.Errorf(".env missing PUID: %s", envFile)
	}
	if !strings.Contains(string(envFile), `SECRET_TOKEN="va\"lue with spaces"`) {
		t.Errorf(".env quoting wrong: %s", envFile)
	}
	fi, _ := os.Stat(filepath.Join(dir, "apps", "sonarr", ".env"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %v, want 0600", fi.Mode().Perm())
	}

	// data dir created for unnamed-host volume
	if _, err := os.Stat(filepath.Join(dir, "data", "sonarr", "config")); err != nil {
		t.Error("volume data dir not created")
	}
	// host-path volume must NOT create anything under data/
	if _, err := os.Stat(filepath.Join(dir, "data", "sonarr", "media")); err == nil {
		t.Error("host-path volume should not create a data dir")
	}
}

// files/ mirrors the spec: a File dropped from it must not survive as a stale
// rendered copy still mounted by nothing — and re-rendering must leave no
// atomic-write temp files behind in any of the artifact directories.
func TestRenderReconcilesFiles(t *testing.T) {
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	spec := &AppSpec{
		Source: Source{Type: "image", Image: "x:latest"},
		Files: []File{
			{Name: "keep.conf", Mount: "/etc/keep.conf", Content: "v1\n"},
			{Name: "drop.conf", Mount: "/etc/drop.conf", Content: "bye\n"},
		},
	}
	if err := m.Render("app", spec, map[string]string{"K": "v"}, spec.Source.Image, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	filesDir := filepath.Join(dir, "apps", "app", "files")

	spec.Files = []File{{Name: "keep.conf", Mount: "/etc/keep.conf", Content: "v2\n"}}
	if err := m.Render("app", spec, map[string]string{"K": "v"}, spec.Source.Image, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filesDir, "drop.conf")); err == nil {
		t.Error("drop.conf survived its removal from the spec")
	}
	kept, err := os.ReadFile(filepath.Join(filesDir, "keep.conf"))
	if err != nil || string(kept) != "v2\n" {
		t.Errorf("keep.conf = %q, %v; want updated content", kept, err)
	}

	// No spec files at all: earlier renders' files must still be swept.
	spec.Files = nil
	if err := m.Render("app", spec, map[string]string{"K": "v"}, spec.Source.Image, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filesDir); err == nil && len(entries) != 0 {
		t.Errorf("files/ still holds %d entries after the spec dropped them all", len(entries))
	}

	// The atomic writes must not litter: every entry in the app dir is a real
	// artifact, none a leftover temp file.
	entries, err := os.ReadDir(filepath.Join(dir, "apps", "app"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "compose.yaml", ".env", "files":
		default:
			t.Errorf("unexpected leftover in app dir: %s", e.Name())
		}
	}
}

func TestRenderPorts(t *testing.T) {
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	spec := &AppSpec{
		Source:    Source{Type: "image", Image: "adguard/adguardhome:latest"},
		Ports:     []Port{{Port: 53, Protocol: "tcp"}, {Port: 53, Protocol: "udp"}},
		Processes: []Process{{Name: "worker", Command: []string{"work"}}},
	}
	if err := m.Render("adguard", spec, nil, spec.Source.Image, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(filepath.Join(dir, "apps", "adguard", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c := string(compose)
	for _, want := range []string{"published: 53", "protocol: udp", "protocol: tcp", "target: 53"} {
		if !strings.Contains(c, want) {
			t.Errorf("compose.yaml missing %q\n%s", want, c)
		}
	}
	// Only the main service publishes; the worker must not.
	if strings.Count(c, "published: 53") != 2 {
		t.Errorf("expected exactly two published port entries (main service only)\n%s", c)
	}
}

func TestRenderMultiProcess(t *testing.T) {
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	spec := &AppSpec{
		Source:    Source{Type: "git", Repo: "ssh://git@example.com/x.git"},
		Processes: []Process{{Name: "worker", Command: []string{"bundle", "exec", "ruby", "bin/jobs"}}},
	}
	if err := m.Render("manor", spec, map[string]string{"RAILS_ENV": "production"}, "fog/manor:abc123def456", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	compose, _ := os.ReadFile(filepath.Join(dir, "apps", "manor", "compose.yaml"))
	c := string(compose)
	for _, want := range []string{
		"manor-worker:", "fog.process: worker", "image: fog/manor:abc123def456",
		"- bundle", "- bin/jobs",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose.yaml missing %q\n%s", want, c)
		}
	}
	// Worker must not carry the app's DNS alias.
	workerSection := c[strings.Index(c, "manor-worker:"):]
	if strings.Contains(workerSection, "aliases") {
		t.Error("worker should not have a network alias")
	}
}

func TestRenderAux(t *testing.T) {
	dir := t.TempDir()
	m := &Materializer{DataDir: dir}
	spec := &AppSpec{
		Source: Source{Type: "image", Image: "ghcr.io/paperless-ngx/paperless-ngx:latest"},
		Aux: []AuxService{{
			Name: "broker", Image: "docker.io/valkey/valkey:9-alpine",
			Env:     map[string]string{"EXAMPLE_REF": "${APP_SECRET}"},
			Volumes: []Volume{{Name: "redisdata", Mount: "/data"}},
			ShmSize: "128mb",
		}},
	}
	if err := m.Render("paperless", spec, map[string]string{"APP_SECRET": "s3cret"}, spec.Source.Image, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	compose, _ := os.ReadFile(filepath.Join(dir, "apps", "paperless", "compose.yaml"))
	c := string(compose)
	for _, want := range []string{
		"broker:",
		"image: docker.io/valkey/valkey:9-alpine",
		"fog.aux: broker",
		filepath.Join(dir, "data", "paperless", "redisdata") + ":/data",
		"shm_size: 128mb",
		"private:", // per-app network exists and both services join it
		"depends_on:",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose.yaml missing %q\n%s", want, c)
		}
	}
	// The sidecar must be private: no fog network, no shared .env.
	var doc struct {
		Services map[string]struct {
			EnvFile  []string       `yaml:"env_file"`
			Networks map[string]any `yaml:"networks"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		t.Fatal(err)
	}
	broker := doc.Services["broker"]
	if _, ok := broker.Networks["fog"]; ok {
		t.Error("aux service must not join the shared fog network")
	}
	// The sidecar gets its own env file, with ${APP_SECRET} expanded by fog
	// from the app's environment — never the app's whole .env.
	if len(broker.EnvFile) != 1 || broker.EnvFile[0] != "aux-broker.env" {
		t.Errorf("aux env_file = %v, want its own", broker.EnvFile)
	}
	auxEnv, _ := os.ReadFile(filepath.Join(dir, "apps", "paperless", "aux-broker.env"))
	if string(auxEnv) != "EXAMPLE_REF=\"s3cret\"\n" {
		t.Errorf("aux env = %q", auxEnv)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "apps", "paperless", "aux-broker.env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("aux env mode %v", fi.Mode().Perm())
	}
	if _, ok := doc.Services["paperless"].Networks["private"]; !ok {
		t.Error("main service must join the private network")
	}
	// The secret value itself must never appear in world-readable compose.yaml.
	if strings.Contains(c, "s3cret") {
		t.Error("secret value leaked into compose.yaml")
	}

	// An aux named like the app collides with its compose service key.
	bad := &AppSpec{
		Source: Source{Type: "image", Image: "x:latest"},
		Aux:    []AuxService{{Name: "paperless", Image: "y:latest"}},
	}
	if err := m.Render("paperless", bad, nil, "x:latest", "", io.Discard); err == nil {
		t.Error("aux name colliding with app name should fail to render")
	}
}

func TestAuxSpecValidate(t *testing.T) {
	ok := &AppSpec{
		Source: Source{Type: "image", Image: "x:latest"},
		Aux: []AuxService{{Name: "db", Image: "postgres:16",
			Volumes: []Volume{{Name: "pgdata", Mount: "/var/lib/postgresql/data"}}}},
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid aux spec rejected: %v", err)
	}
	for _, bad := range []*AppSpec{
		{Source: Source{Type: "image", Image: "x"}, Aux: []AuxService{{Name: "db"}}},                                                                                                             // no image
		{Source: Source{Type: "image", Image: "x"}, Aux: []AuxService{{Name: "Bad Name", Image: "y"}}},                                                                                           // bad name
		{Source: Source{Type: "image", Image: "x"}, Aux: []AuxService{{Name: "db", Image: "y"}, {Name: "db", Image: "z"}}},                                                                       // dup aux
		{Source: Source{Type: "image", Image: "x"}, Processes: []Process{{Name: "db", Command: []string{"run"}}}, Aux: []AuxService{{Name: "db", Image: "y"}}},                                   // clashes with process
		{Source: Source{Type: "image", Image: "x"}, Volumes: []Volume{{Name: "data", Mount: "/a"}}, Aux: []AuxService{{Name: "db", Image: "y", Volumes: []Volume{{Name: "data", Mount: "/b"}}}}}, // dup volume across app+aux
		{Source: Source{Type: "image", Image: "x"}, Aux: []AuxService{{Name: "db", Image: "y", Env: map[string]string{"bad key": "v"}}}},                                                         // bad env key
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("expected validation error for %+v", bad.Aux)
		}
	}
}

func TestValidateGitRepo(t *testing.T) {
	valid := []string{
		"ssh://git@git.example.com/you/app.git",
		"ssh://git@github.com/owner/repo.git",
		"ssh://gitea@gitea.lan:2222/owner/repo.git",
		"gitea@gitea.lan:matthew/manor.git",
		"/var/repos/local.git",
		"https://github.com/fogapp/fin.git",
		"https://gitea.lan:3000/owner/repo",
	}
	for _, v := range valid {
		if err := validateGitRepo(v); err != nil {
			t.Errorf("%q should be valid: %v", v, err)
		}
	}
	invalid := map[string]string{
		"":                                        "required",
		"http://gitea.lan/matthew/manor.git":      "unencrypted",
		"https://tok@github.com/owner/repo.git":   "credentials",
		"https://u:p@github.com/owner/repo.git":   "credentials",
		"https://github.com/owner/repo.git?x=1":   "query",
		"https://github.com":                      "missing the repository path",
		"git://host/repo.git":                     "unauthenticated",
		"ssh://gitea.lan/matthew/manor.git":       "missing its user",
		"ssh://gitea@gitea.lan":                   "missing the repository path",
		"ssh://gitea@gitea.lan/ matthew/repo.git": "whitespace",
		"gitea.lan/matthew/manor.git":             "Unrecognized",
	}
	for repo, wantSubstr := range invalid {
		err := validateGitRepo(repo)
		if err == nil {
			t.Errorf("%q should be invalid", repo)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(wantSubstr)) {
			t.Errorf("%q → %q, want mention of %q", repo, err.Error(), wantSubstr)
		}
	}
}

func TestGitSpecValidate(t *testing.T) {
	spec := &AppSpec{
		Source:    Source{Type: "git", Repo: "ssh://git@gitea.lan/matthew/manor.git", BuildArgs: map[string]string{"GIT_REV": "{{commit}}"}},
		Release:   []string{"bundle", "exec", "rails", "db:migrate"},
		Processes: []Process{{Name: "worker", Command: []string{"bin/jobs"}}},
		Needs:     &Needs{Postgres: &PostgresNeed{Databases: []string{"manor_production"}, Extensions: []string{"citext", "pg_trgm"}, Inject: map[string]string{"DATABASE_HOST": "host"}}},
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid git spec rejected: %v", err)
	}
	spec.Needs.Postgres.Inject["X"] = "bogus"
	if err := spec.Validate(); err == nil {
		t.Fatal("bogus inject field accepted")
	}
	delete(spec.Needs.Postgres.Inject, "X")
	spec.Needs.Postgres.Databases = []string{"bad-name"}
	if err := spec.Validate(); err == nil {
		t.Fatal("invalid pg identifier accepted")
	}
}

// Shared images survive an app's deletion; exclusive ones are reclaimed.
func TestUnreferencedImages(t *testing.T) {
	victim := &AppSpec{
		Source: Source{Type: "image", Image: "immich:release"},
		Aux: []AuxService{
			{Name: "redis", Image: "valkey:9"},
			{Name: "db", Image: "vector-pg:14"},
		},
	}
	others := []*AppSpec{
		{Source: Source{Type: "image", Image: "paperless:latest"}, Aux: []AuxService{{Name: "broker", Image: "valkey:9"}}},
		{Source: Source{Type: "git", Repo: "ssh://x/y.git"}},
	}
	got := unreferencedImages(victim, others)
	want := map[string]bool{"immich:release": true, "vector-pg:14": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, ref := range got {
		if !want[ref] {
			t.Errorf("unexpected image %q", ref)
		}
	}

	// Git apps contribute no pulled images.
	if refs := specImages(&AppSpec{Source: Source{Type: "git", Repo: "r", Image: "ignored"}}); len(refs) != 0 {
		t.Errorf("git spec leaked images: %v", refs)
	}
}

func TestHostPaths(t *testing.T) {
	spec := AppSpec{
		Volumes: []Volume{
			{Name: "storage", Mount: "/rails/storage"},
			{Name: "media", Mount: "/data/media", Host: "/mnt/nas/media"},
		},
		Aux: []AuxService{{
			Name: "ml", Image: "x",
			Volumes: []Volume{{Name: "cache", Mount: "/cache", Host: "/mnt/nas/ml-cache"}},
		}},
	}
	got := spec.HostPaths("")
	want := []string{"/mnt/nas/media", "/mnt/nas/ml-cache"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("HostPaths() = %v, want %v", got, want)
	}
	if paths := (&AppSpec{}).HostPaths(""); len(paths) != 0 {
		t.Errorf("empty spec: got %v", paths)
	}
}

// A Files volume names a folder relative to the Files root and resolves
// against whatever the root is at the time — so it is absent from
// HostPaths without a root and present, absolute, with one.
func TestFilesVolume(t *testing.T) {
	spec := &AppSpec{
		Source:  Source{Type: "image", Image: "x"},
		Volumes: []Volume{{Name: "data", Mount: "/data"}, {Name: "books", Mount: "/books", Files: "Apps/Books"}},
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid Files volume rejected: %v", err)
	}
	if paths := spec.HostPaths(""); len(paths) != 0 {
		t.Errorf("no root: HostPaths = %v, want none", paths)
	}
	if paths := spec.HostPaths("/srv/files"); len(paths) != 1 || paths[0] != "/srv/files/Apps/Books" {
		t.Errorf("HostPaths = %v", paths)
	}
	if fv := spec.FilesVolumes(); len(fv) != 1 || fv[0].Name != "books" {
		t.Errorf("FilesVolumes = %+v", fv)
	}
	if !spec.Volumes[0].Managed() || spec.Volumes[1].Managed() {
		t.Error("Managed() wrong")
	}
	for _, bad := range []Volume{
		{Name: "b", Mount: "/b", Files: "Apps/Books", Host: "/mnt/books"},
		{Name: "b", Mount: "/b", Files: "/Apps/Books"},
		{Name: "b", Mount: "/b", Files: "../etc"},
		{Name: "b", Mount: "/b", Files: "Apps//Books"},
		{Name: "b", Mount: "/b", Files: ".fog/trash"},
		{Name: "b", Mount: "/b", Files: "Apps/."},
	} {
		s := &AppSpec{Source: Source{Type: "image", Image: "x"}, Volumes: []Volume{bad}}
		if err := s.Validate(); err == nil {
			t.Errorf("accepted Files=%q Host=%q", bad.Files, bad.Host)
		}
	}
}

// Rendering a Files volume asks Files for the folder and binds the
// resolved path; without Files the deploy fails naming the volume, rather
// than binding a relative path docker would turn into a named volume.
func TestRenderFilesVolume(t *testing.T) {
	dir := t.TempDir()
	filesRoot := filepath.Join(dir, "files")
	var ensured []string
	m := &Materializer{
		DataDir:   dir,
		FilesRoot: func() string { return filesRoot },
		EnsureFilesDir: func(rel string) error {
			ensured = append(ensured, rel)
			return os.MkdirAll(filepath.Join(filesRoot, rel), 0o755)
		},
	}
	spec := &AppSpec{
		Source:  Source{Type: "image", Image: "img"},
		Volumes: []Volume{{Name: "books", Mount: "/books", Files: "Apps/Books"}},
	}
	if err := m.Render("bookorbit", spec, nil, "img", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	compose, _ := os.ReadFile(filepath.Join(dir, "apps", "bookorbit", "compose.yaml"))
	if want := filepath.Join(filesRoot, "Apps", "Books") + ":/books"; !strings.Contains(string(compose), want) {
		t.Errorf("compose.yaml missing %q\n%s", want, compose)
	}
	if len(ensured) != 1 || ensured[0] != "Apps/Books" {
		t.Errorf("EnsureFilesDir calls = %v", ensured)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "bookorbit", "books")); err == nil {
		t.Error("a Files volume must not create a managed data dir")
	}

	none := &Materializer{DataDir: dir}
	err := none.Render("bookorbit", spec, nil, "img", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "books") || !strings.Contains(err.Error(), "Drive") {
		t.Errorf("render without Drive: err = %v", err)
	}
}

// Extension names may carry a hyphen (uuid-ossp) — CREATE EXTENSION quotes
// them. Database names never may: they reach CREATE DATABASE unquoted.
func TestSpecValidatePostgresIdentifiers(t *testing.T) {
	base := func(pg *PostgresNeed) *AppSpec {
		return &AppSpec{
			Source: Source{Type: "image", Image: "ghcr.io/bookorbit/bookorbit:latest"},
			Routes: []Route{{Port: 3000}},
			Needs:  &Needs{Postgres: pg},
		}
	}
	ok := base(&PostgresNeed{Databases: []string{"bookorbit"}, Extensions: []string{"uuid-ossp", "pg_trgm", "vector"}})
	if err := ok.Validate(); err != nil {
		t.Errorf("hyphenated extension rejected: %v", err)
	}
	for _, bad := range []*AppSpec{
		base(&PostgresNeed{Databases: []string{"book-orbit"}}),
		base(&PostgresNeed{Databases: []string{"bookorbit"}, Extensions: []string{"-ossp"}}),
		base(&PostgresNeed{Databases: []string{"bookorbit"}, Extensions: []string{`uuid"ossp`}}),
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v should be invalid", bad.Needs.Postgres)
		}
	}
}

// The backup predicates are nil-safe all the way down, so a check can ask
// them of an app with no revision yet, and they agree with the spec.
func TestBackupPredicates(t *testing.T) {
	for _, c := range []struct {
		spec               *AppSpec
		enabled, delegated bool
	}{
		{nil, false, false},
		{&AppSpec{}, false, false},
		{&AppSpec{Backup: &BackupSpec{}}, false, false},
		{&AppSpec{Backup: &BackupSpec{Enabled: true}}, true, false},
		{&AppSpec{Backup: &BackupSpec{Enabled: true, Mode: BackupModeVolumes}}, true, false},
		{&AppSpec{Backup: &BackupSpec{Enabled: true, Mode: BackupModeDelegated}}, true, true},
		{&AppSpec{Backup: &BackupSpec{Mode: BackupModeDelegated}}, false, true},
	} {
		if got := c.spec.BackupsEnabled(); got != c.enabled {
			t.Errorf("%+v: BackupsEnabled = %v", c.spec, got)
		}
		if got := c.spec.DelegatedBackups(); got != c.delegated {
			t.Errorf("%+v: DelegatedBackups = %v", c.spec, got)
		}
	}
	if (Source{}).BranchOrDefault() != "main" || (Source{Branch: "dev"}).BranchOrDefault() != "dev" {
		t.Error("BranchOrDefault")
	}
}
