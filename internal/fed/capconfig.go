package fed

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// CapConfig reads one capability row of a peer and decodes its config into
// T. ok says whether the row exists; err is the store's failure or the
// config's failure to decode, in which case T holds whatever did decode.
func CapConfig[T any](st *store.Store, peerID int64, capability, direction string) (cfg T, ok bool, err error) {
	raw, ok, err := st.FedCapabilityConfig(peerID, capability, direction)
	if err != nil || !ok {
		return cfg, ok, err
	}
	if raw != "" {
		err = json.Unmarshal([]byte(raw), &cfg)
	}
	return cfg, true, err
}

// AppScope is the app scope of an apps.directory or apps.introduce grant:
// which of this household's apps the grant covers. Its config has exactly
// two shapes, the two the peer settings produce:
//
//	{}                     every app (for apps.introduce, every contracted app)
//	{"apps": ["feed", …]}  those apps and no others; [] is none
//
// "Absent" and "empty" are different statements. Treating an empty list as
// "all" meant unticking the last app on the Selected… list published every
// app instead of none.
type AppScope struct {
	all  bool
	apps []string
}

// Covers reports whether the scope includes the app named, compared the way
// people type app names.
func (s AppScope) Covers(app string) bool {
	if s.all {
		return true
	}
	for _, a := range s.apps {
		if strings.EqualFold(a, app) {
			return true
		}
	}
	return false
}

// Without is the config of this scope with app taken out of its list, and
// whether that changed anything. "Every app" names no app, so it is left
// as it is: it covers whatever apps there are, as it always did.
func (s AppScope) Without(app string) (string, bool) {
	if s.all {
		return "", false
	}
	kept := make([]string, 0, len(s.apps))
	for _, a := range s.apps {
		if !strings.EqualFold(a, app) {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(s.apps) {
		return "", false
	}
	raw, _ := json.Marshal(map[string][]string{"apps": kept})
	return string(raw), true
}

// ParseAppScope reads a grant's config strictly. Anything other than the
// two shapes — an unknown field, "apps" as anything but a list of names — is
// an error, which a caller enforcing the grant treats as covering nothing: a
// scope that can't be read is never widened to "all".
func ParseAppScope(raw string) (AppScope, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AppScope{all: true}, nil
	}
	var cfg struct {
		Apps *[]string `json:"apps"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return AppScope{}, errors.New(`an app scope is {} for every app, or {"apps": ["name", …]}`)
	}
	if dec.More() {
		return AppScope{}, errors.New("an app scope is one JSON object")
	}
	if cfg.Apps == nil {
		return AppScope{all: true}, nil
	}
	for _, a := range *cfg.Apps {
		if strings.TrimSpace(a) == "" {
			return AppScope{}, errors.New("an app scope names apps; an empty name names none")
		}
	}
	return AppScope{apps: *cfg.Apps}, nil
}
