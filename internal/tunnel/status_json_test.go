package tunnel

import (
	"encoding/json"
	"testing"

	"gitea.lan/matthew/fog/internal/supervise"
)

// The supervisor fields are embedded, so the JSON keeps every key at the
// top level, spelled as before.
func TestStatusJSON(t *testing.T) {
	st := Status{
		Status: supervise.Status{Installed: true, Installing: true, InstallError: "x",
			Running: true, Pid: 7, Restarts: 1, LastError: "y"},
		TokenConfigured: true,
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"installed":true,"installing":true,"installError":"x","running":true,"pid":7,"restarts":1,"lastError":"y","tokenConfigured":true}`
	if string(b) != want {
		t.Fatalf("json = %s\nwant %s", b, want)
	}
}
