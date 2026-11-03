package apps

// What a git app says about integrations in its own repo
// (docs/integrations-contract.md, "Declaring in the repo"). The catalog and
// an imported manifest only seed a spec at install; a repo's fog.yaml is
// read at every deploy, so an offer or a want the app adds reaches the
// installed app with its next push.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Declared is the integrations block of a repo's fog.yaml:
//
//	integrations:
//	  port: 80            # optional; else the spec's connection, else its health port
//	  offers: [...]       # as connection.offers
//	  wants: [...]        # as a spec's wants
type Declared struct {
	Port   int     `json:"port,omitempty"`
	Path   string  `json:"path,omitempty"`
	Offers []Offer `json:"offers,omitempty"`
	Wants  []Want  `json:"wants,omitempty"`
}

// DeclaredFrom reads the block as fog.yaml decoded it. A misspelt field is
// an error, not an offer quietly missing a variable.
func DeclaredFrom(raw map[string]any) (*Declared, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d Declared
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("integrations: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("integrations: %w", err)
	}
	return &d, nil
}

func (d *Declared) Validate() error {
	if d.Port < 0 || d.Port > 65535 {
		return fmt.Errorf("port %d out of range", d.Port)
	}
	if d.Path != "" && !strings.HasPrefix(d.Path, "/") {
		return fmt.Errorf("path %q must start with /", d.Path)
	}
	if err := validateOffers(d.Offers); err != nil {
		return err
	}
	return validateWants(d.Wants)
}

// ParseDeclared reads a recorded block back; empty or unreadable is none.
func ParseDeclared(raw string) *Declared {
	if raw == "" {
		return nil
	}
	var d Declared
	if json.Unmarshal([]byte(raw), &d) != nil {
		return nil
	}
	return &d
}

// JSON is the block as recorded, or "" for none.
func (d *Declared) JSON() string {
	if d == nil || (len(d.Offers) == 0 && len(d.Wants) == 0) {
		return ""
	}
	b, _ := json.Marshal(d)
	return string(b)
}

// EffectiveConnection is how other apps reach this one for integrations:
// the spec's connection, plus the offers the repo declares. The spec is the
// operator's, so an offer it makes under the same id wins. Nil when neither
// offers anything and the spec has no connection.
func EffectiveConnection(spec *AppSpec, d *Declared) *Connection {
	if d == nil || len(d.Offers) == 0 {
		return spec.Connection
	}
	var c Connection
	if spec.Connection != nil {
		c = *spec.Connection
	} else {
		c.Port, c.Path = d.Port, d.Path
		if c.Port == 0 {
			c.Port = spec.HealthPort()
		}
	}
	c.Offers = append([]Offer{}, c.Offers...)
	for _, o := range d.Offers {
		if c.Offer(o.ID) == nil {
			c.Offers = append(c.Offers, o)
		}
	}
	return &c
}

// EffectiveWants is the spec's wants plus the ones the repo declares; the
// spec's win for the same offer or app.
func EffectiveWants(spec *AppSpec, d *Declared) []Want {
	out := append([]Want{}, spec.Wants...)
	if d == nil {
		return out
	}
	for _, w := range d.Wants {
		if !slices.ContainsFunc(out, func(x Want) bool { return x.key() == w.key() }) {
			out = append(out, w)
		}
	}
	return out
}
