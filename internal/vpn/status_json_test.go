package vpn

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"gitea.lan/matthew/fog/internal/supervise"
)

// The supervisor fields are embedded, so the JSON the Away access page reads
// keeps every key at the top level, spelled as before.
func TestStatusJSONKeys(t *testing.T) {
	st := Status{
		Status: supervise.Status{Installed: true, Installing: true, InstallError: "x",
			Running: true, Pid: 7, Restarts: 1, LastError: "y"},
		Version: "1.2", UpError: "z", KeyConfigured: true, LoggedIn: true,
		Route: "10.0.0.1/32", RouteError: "r", Notes: []string{"n"},
		StatusKnown: true, Snapshot: &Snapshot{},
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"installError", "installed", "installing", "keyConfigured", "lastError",
		"loggedIn", "notes", "pid", "restarts", "route", "routeError", "running", "snapshot",
		"statusKnown", "upError", "version"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v\nwant %v", keys, want)
	}
}
