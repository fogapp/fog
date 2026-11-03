package apps

import (
	"strings"
	"testing"
)

const arrCompose = `
services:
  sonarr:
    image: lscr.io/linuxserver/sonarr:latest
    container_name: sonarr
    environment:
      - PUID=1000
      - PGID=1000
      - TZ=America/Los_Angeles
    volumes:
      - /opt/sonarr/config:/config
      - /mnt/media:/data
    ports:
      - "8989:8989"
    restart: unless-stopped
    devices:
      - /dev/dri:/dev/dri
`

func TestImportCompose(t *testing.T) {
	services, err := ImportCompose(arrCompose)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	s := services[0]
	if s.Name != "sonarr" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Spec.Source.Image != "lscr.io/linuxserver/sonarr:latest" {
		t.Errorf("image = %q", s.Spec.Source.Image)
	}
	if s.Spec.Env["PUID"] != "1000" || s.Spec.Env["TZ"] != "America/Los_Angeles" {
		t.Errorf("env = %v", s.Spec.Env)
	}
	if len(s.Spec.Volumes) != 2 || s.Spec.Volumes[0].Host != "/opt/sonarr/config" || s.Spec.Volumes[0].Mount != "/config" {
		t.Errorf("volumes = %+v", s.Spec.Volumes)
	}
	if len(s.Spec.Routes) != 1 || s.Spec.Routes[0].Port != 8989 {
		t.Errorf("routes = %+v", s.Spec.Routes)
	}
	// devices is unsupported → must surface as a warning, not vanish, and
	// one that says what it would have handed the container.
	found := false
	for _, w := range s.Warnings {
		if strings.HasPrefix(w, "devices dropped — it passes host devices through") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected devices warning, got %v", s.Warnings)
	}
	if err := s.Spec.Validate(); err != nil {
		t.Errorf("imported spec should validate: %v", err)
	}
}

func TestImportComposeEnvMap(t *testing.T) {
	services, err := ImportCompose("services:\n  app:\n    image: x\n    environment:\n      KEY: value\n      NUM: 5\n")
	if err != nil {
		t.Fatal(err)
	}
	if services[0].Spec.Env["KEY"] != "value" || services[0].Spec.Env["NUM"] != "5" {
		t.Errorf("env = %v", services[0].Spec.Env)
	}
}
