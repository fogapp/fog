// Package sample exercises tsgen's mapping of encoding/json rules.
package sample

import "time"

// Kind is a string type with constants: it becomes a union.
type Kind string

const (
	KindA Kind = "a"
	KindB Kind = "b"
)

type Base struct {
	ID    int64  `json:"id"`
	Shade string `json:"shade"`
}

// Thing is the root.
type Thing struct {
	Base
	// Shade here is shallower than Base's, so it wins.
	Shade    int            `json:"shade"`
	Name     string         `json:"name"`
	Note     string         `json:"note,omitempty"`
	Kind     Kind           `json:"kind"`
	Mode     string         `json:"mode" ts:"\"x\" | \"y\""`
	When     time.Time      `json:"when"`
	Child    *Child         `json:"child"`
	Maybe    *Child         `json:"maybe,omitempty"`
	Always   Child          `json:"always,omitempty"`
	Tags     []string       `json:"tags"`
	Attrs    map[string]int `json:"attrs"`
	Count    int64          `json:"count,string"`
	Raw      []byte         `json:"raw"`
	Hidden   string         `json:"-"`
	Untagged bool
	inner    string
	Nested   struct{ X int }   `json:"nested"`
	ByName   map[string]*Child `json:"byName,omitempty"`
}

type Child struct {
	Value float64 `json:"value"`
}

type settingKey string

const (
	localKey            = "local"
	kindKey  settingKey = "kind"
)

// settings are the editable keys.
var settings = map[settingKey]func(string) bool{
	"plain": nil,
	// Doc above an entry carries over.
	"documented": nil,
	localKey:     nil, // a local constant, with a trailing comment
	kindKey:      nil, // a typed constant
	"dotted.key": nil,
	"pass":       nil, // stored sealed
	"multi": func(v string) bool { // a func value spanning lines
		// not the next entry's doc
		return v == ""
	},
}

var secrets = map[string]bool{"pass": true}

var strays = map[string]bool{"missing": true}
