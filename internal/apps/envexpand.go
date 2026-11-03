package apps

import (
	"sort"
	"strings"
)

// expandEnvRefs expands what a sidecar's env value may say of the app's
// environment, the forms compose would have: $VAR, ${VAR}, ${VAR:-default},
// ${VAR-default}, and $$ for a literal $. An unknown variable is empty, as
// in compose. Done by fog, so compose never interpolates anything.
func expandEnvRefs(v string, env map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c != '$' || i+1 >= len(v) {
			b.WriteByte(c)
			continue
		}
		switch n := v[i+1]; {
		case n == '$':
			b.WriteByte('$')
			i++
		case n == '{':
			end := strings.IndexByte(v[i+2:], '}')
			if end < 0 {
				b.WriteString(v[i:])
				return b.String()
			}
			expr := v[i+2 : i+2+end]
			name, def, mode := expr, "", ""
			if j := strings.Index(expr, ":-"); j >= 0 {
				name, def, mode = expr[:j], expr[j+2:], ":-"
			} else if j := strings.IndexByte(expr, '-'); j >= 0 {
				name, def, mode = expr[:j], expr[j+1:], "-"
			}
			val, ok := env[name]
			switch {
			case mode == ":-" && val == "":
				val = def
			case mode == "-" && !ok:
				val = def
			}
			b.WriteString(val)
			i += 2 + end
		case n == '_' || n >= 'A' && n <= 'Z' || n >= 'a' && n <= 'z':
			j := i + 1
			for j < len(v) && (v[j] == '_' || v[j] >= 'A' && v[j] <= 'Z' || v[j] >= 'a' && v[j] <= 'z' || v[j] >= '0' && v[j] <= '9') {
				j++
			}
			b.WriteString(env[v[i+1:j]])
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func sortedEnvNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
