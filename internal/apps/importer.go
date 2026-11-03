package apps

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ImportCompose converts a docker-compose file into Fog app specs — the
// bridge from the wider self-hosted ecosystem's documentation into Fog's
// model. Fields Fog doesn't model are reported as warnings, never silently
// dropped.
type ImportedService struct {
	Name     string   `json:"name"`
	Spec     AppSpec  `json:"spec"`
	Warnings []string `json:"warnings"`
}

func ImportCompose(text string) ([]ImportedService, error) {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	if len(doc.Services) == 0 {
		return nil, fmt.Errorf("no services found")
	}

	known := map[string]bool{
		"image": true, "environment": true, "volumes": true, "ports": true,
		"command": true, "restart": true, "container_name": true, "labels": true,
		"networks": true, "depends_on": true, "hostname": true, "env_file": true,
		"healthcheck": true, "mem_limit": true,
	}

	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []ImportedService
	for _, svcName := range names {
		svc := doc.Services[svcName]
		imp := ImportedService{Name: sanitizeName(svcName)}
		spec := AppSpec{Source: Source{Type: "image"}, Env: map[string]string{}}

		if img, ok := svc["image"].(string); ok {
			spec.Source.Image = img
		} else {
			imp.Warnings = append(imp.Warnings, "no image (build-based services must be created as git apps)")
		}

		// environment: map or list of K=V
		switch envv := svc["environment"].(type) {
		case map[string]any:
			for k, v := range envv {
				spec.Env[k] = toString(v)
			}
		case []any:
			for _, item := range envv {
				str := toString(item)
				if k, v, ok := strings.Cut(str, "="); ok {
					spec.Env[k] = v
				}
			}
		}

		// volumes: "src:dst[:ro]" strings
		if vols, ok := svc["volumes"].([]any); ok {
			for i, item := range vols {
				str, ok := item.(string)
				if !ok {
					imp.Warnings = append(imp.Warnings, "long-form volume entry skipped — add it manually")
					continue
				}
				parts := strings.Split(str, ":")
				switch len(parts) {
				case 1:
					imp.Warnings = append(imp.Warnings, "anonymous volume "+str+" mapped to managed volume")
					spec.Volumes = append(spec.Volumes, Volume{Name: volName(parts[0], i), Mount: parts[0]})
				default:
					src, dst := parts[0], parts[1]
					v := Volume{Name: volName(dst, i), Mount: dst}
					if strings.HasPrefix(src, "/") {
						v.Host = src // explicit host path preserved
						v.ReadOnly = len(parts) > 2 && slices.Contains(strings.Split(parts[2], ","), "ro")
						// Never acknowledged on the way in: whoever wrote
						// the compose file isn't the one handing over the
						// machine. The operator adds the acknowledgement,
						// or the create is refused with the reason.
						if why := DangerousHostPath(src, v.ReadOnly); why != "" {
							imp.Warnings = append(imp.Warnings, fmt.Sprintf(
								"%s mounts %s — %s. Fog won't create it unless you acknowledge that path (acknowledgeHostAccess), or drop the volume", dst, cleanHostPath(src), why))
						}
					}
					// named docker volumes become Fog-managed dirs
					spec.Volumes = append(spec.Volumes, v)
				}
			}
		}

		// ports: first published port becomes the route
		if ports, ok := svc["ports"].([]any); ok && len(ports) > 0 {
			str := toString(ports[0])
			seg := strings.Split(strings.Split(str, "/")[0], ":")
			containerPort := seg[len(seg)-1]
			if p, err := strconv.Atoi(containerPort); err == nil {
				spec.Routes = []Route{{Port: p}}
			}
			if len(ports) > 1 {
				imp.Warnings = append(imp.Warnings, fmt.Sprintf("%d additional port mappings ignored (Fog routes HTTP through the proxy)", len(ports)-1))
			}
		}

		switch cmd := svc["command"].(type) {
		case string:
			spec.Command = strings.Fields(cmd)
		case []any:
			for _, c := range cmd {
				spec.Command = append(spec.Command, toString(c))
			}
		}
		if ml, ok := svc["mem_limit"].(string); ok {
			spec.MemoryLimit = ml
		}

		for k := range svc {
			if why, ok := privilegedKeys[k]; ok {
				imp.Warnings = append(imp.Warnings, fmt.Sprintf("%s dropped — %s; fog doesn't run containers with it", k, why))
				continue
			}
			if !known[k] {
				imp.Warnings = append(imp.Warnings, "unsupported key ignored: "+k)
			}
		}
		sort.Strings(imp.Warnings)
		imp.Spec = spec
		out = append(out, imp)
	}
	return out, nil
}

// privilegedKeys are compose settings that widen what a container may do
// to the host. Fog's spec has none of them, so an import drops them — and
// says so in words that make the drop impossible to mistake for a nit.
var privilegedKeys = map[string]string{
	"privileged":          "it gives the container every device and capability of the host",
	"cap_add":             "it adds kernel capabilities",
	"devices":             "it passes host devices through",
	"device_cgroup_rules": "it opens host devices",
	"network_mode":        "host networking puts the container on the host's own network stack",
	"pid":                 "sharing the host's process namespace exposes every process",
	"ipc":                 "sharing the host's IPC namespace exposes its shared memory",
	"userns_mode":         "it turns off user-namespace remapping",
	"security_opt":        "it loosens the container's security profile",
	"cgroup_parent":       "it places the container outside fog's control group",
	"sysctls":             "it changes kernel parameters",
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func volName(mount string, i int) string {
	base := sanitizeName(strings.TrimPrefix(mount, "/"))
	base = strings.ReplaceAll(base, "/", "-")
	if base == "" {
		base = fmt.Sprintf("vol%d", i)
	}
	if len(base) > 30 {
		base = base[:30]
	}
	return strings.Trim(base, "-")
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int, int64, float64, bool:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}
