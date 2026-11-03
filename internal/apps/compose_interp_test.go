package apps

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The attack: a spec whose strings compose would interpolate after fog
// checked them — a host path "/srv/x${UNSET-/var/run/docker.sock}" that
// every check reads as a harmless literal, and compose reads as the Docker
// socket. Run through real `docker compose config`, every string must come
// back exactly as fog wrote it; only a sidecar's environment interpolates.
//
// Needs the docker compose CLI (not the daemon); skipped without it.
func TestComposeReadsEveryStringLiterally(t *testing.T) {
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("docker compose not installed")
	}
	host := filepath.Join(t.TempDir(), "x${UNSET-/var/run/docker.sock}")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Materializer{DataDir: t.TempDir()}
	spec := &AppSpec{
		Source:                Source{Type: "image", Image: "x:1"},
		Volumes:               []Volume{{Name: "v", Mount: "/data${UNSET-/etc}", Host: host}},
		AcknowledgeHostAccess: []string{host},
		Command:               []string{"sh", "-c", "echo ${HOME:-home} $$ $PATH"},
		Aux: []AuxService{{
			Name: "db", Image: "postgres${TAG-:latest}",
			Env:     map[string]string{"POSTGRES_PASSWORD": "${DB_PASSWORD}"},
			Command: []string{"${UNSET:?boom}"},
		}},
	}
	env := map[string]string{"DB_PASSWORD": "s3cret", "UNSET_NOT": "x"}
	if err := m.Render("app", spec, env, "img${TAG-:evil}", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "compose", "-f", filepath.Join(m.AppDir("app"), "compose.yaml"), "config", "--format", "json")
	cmd.Dir = m.AppDir("app")
	cmd.Env = append(os.Environ(), "UNSET=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose config: %v\n%s", err, out)
	}
	var doc struct {
		Services map[string]struct {
			Image       string            `json:"image"`
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
			Volumes     []struct {
				Source string `json:"source"`
				Target string `json:"target"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// config prints a compose file, so it writes a literal "$" back as "$$";
	// undone here, what is left is what compose read.
	lit := func(s string) string { return strings.ReplaceAll(s, "$$", "$") }
	for name, svc := range doc.Services {
		svc.Image = lit(svc.Image)
		for i := range svc.Command {
			svc.Command[i] = lit(svc.Command[i])
		}
		for i := range svc.Volumes {
			svc.Volumes[i].Source, svc.Volumes[i].Target = lit(svc.Volumes[i].Source), lit(svc.Volumes[i].Target)
		}
		doc.Services[name] = svc
	}
	app, db := doc.Services["app"], doc.Services["db"]
	if len(app.Volumes) != 1 || app.Volumes[0].Source != host || app.Volumes[0].Target != "/data${UNSET-/etc}" {
		t.Errorf("compose binds %+v, want %s literally", app.Volumes, host)
	}
	if app.Image != "img${TAG-:evil}" || strings.Join(app.Command, " ") != "sh -c echo ${HOME:-home} $$ $PATH" {
		t.Errorf("app read as %q %q", app.Image, app.Command)
	}
	if db.Image != "postgres${TAG-:latest}" || strings.Join(db.Command, " ") != "${UNSET:?boom}" {
		t.Errorf("sidecar read as %q %q", db.Image, db.Command)
	}
	// The one place interpolation is meant: a sidecar's environment.
	if db.Environment["POSTGRES_PASSWORD"] != "s3cret" {
		t.Errorf("sidecar env = %q, want the app's DB_PASSWORD", db.Environment["POSTGRES_PASSWORD"])
	}
}

func TestValidateRefusesDollarInHostPath(t *testing.T) {
	s := &AppSpec{Source: Source{Type: "image", Image: "x"}, Volumes: []Volume{{Name: "v", Mount: "/v", Host: "/srv/x${UNSET-/var/run/docker.sock}"}}}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "$") {
		t.Fatalf("err = %v", err)
	}
}

// The attack: env names that are compose's own settings. In the app's .env
// (which compose used to read as its settings file) — or in fogd's own
// environment — COMPOSE_FILE adds a file of the app's (a privileged
// container with / bound) and COMPOSE_PROJECT_NAME points --remove-orphans
// at another app. Every compose fog runs must read fog's file, as fog's
// project, whatever either says.
func TestComposeCmdIgnoresComposeSettingsFromTheApp(t *testing.T) {
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("docker compose not installed")
	}
	m := &Materializer{DataDir: t.TempDir()}
	spec := &AppSpec{
		Source: Source{Type: "image", Image: "x:1"},
		Files:  []File{{Name: "evil.yaml", Mount: "/x", Content: "services:\n  evil:\n    image: alpine\n    privileged: true\n    volumes: [\"/:/host\"]\n"}},
	}
	// Written as an older fog (or a provider's offer) would have.
	env := map[string]string{"PLAIN": "1"}
	if err := m.Render("app", spec, env, "x:1", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(m.AppDir("app"), ".env"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("COMPOSE_FILE=\"compose.yaml:files/evil.yaml\"\nCOMPOSE_PROJECT_NAME=\"victim\"\nCOMPOSE_PROFILES=\"x\"\n")
	f.Close()
	t.Setenv("COMPOSE_FILE", "compose.yaml:files/evil.yaml")
	t.Setenv("COMPOSE_PROJECT_NAME", "victim")
	cmd := m.ComposeCmd(t.Context(), "app", "config", "--format", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose config: %v\n%s", err, out)
	}
	var doc struct {
		Name     string         `json:"name"`
		Services map[string]any `json:"services"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc.Name != "app" {
		t.Errorf("compose ran as project %q, want app", doc.Name)
	}
	if _, ok := doc.Services["evil"]; ok || len(doc.Services) != 1 {
		t.Errorf("compose read another file: services %v", doc.Services)
	}
}

func TestValidateRefusesToolEnvNames(t *testing.T) {
	for _, k := range []string{"COMPOSE_FILE", "COMPOSE_PROJECT_NAME", "compose_profiles", "DOCKER_HOST", "DOCKER_CONFIG"} {
		s := &AppSpec{Source: Source{Type: "image", Image: "x"}, Env: map[string]string{k: "x"}}
		if err := s.Validate(); err == nil {
			t.Errorf("env %s accepted", k)
		}
		s = &AppSpec{Source: Source{Type: "image", Image: "x"}, Secrets: []string{k}}
		if err := s.Validate(); err == nil {
			t.Errorf("secret %s accepted", k)
		}
		if err := CheckOfferedName(k); err == nil {
			t.Errorf("offered %s accepted", k)
		}
	}
	r := BuildReview("app", &AppSpec{Source: Source{Type: "image", Image: "x"}, Env: map[string]string{"B": "1", "A": "2"},
		Aux: []AuxService{{Name: "db", Env: map[string]string{"P": "x"}}}})
	if fmt.Sprint(r.Env) != "[A B db: P]" {
		t.Errorf("review env = %v", r.Env)
	}
}
