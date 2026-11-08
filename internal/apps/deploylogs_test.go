package apps

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

// The attack: a deploy log written before fog redacted at the source holds
// a build-arg token and a repository password, and the app page, the
// status poll, MCP and feedback reports read it. Every read through the
// engine comes back redacted — with values from any revision, not just the
// newest (the token was rotated out of the spec since).
func TestStoredDeployLogsAreRedactedWhenRead(t *testing.T) {
	st, err := storetest.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := st.CreateApp("app")
	put := func(spec AppSpec) *store.AppRevision {
		b, _ := json.Marshal(spec)
		r, err := st.CreateRevision(a.ID, string(b), "", "t")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	old := put(AppSpec{Source: Source{Type: "git", Repo: "ssh://git:hunter2pass@git.example.com/o/r.git",
		BuildArgs: map[string]string{"NPM_TOKEN": "npm_0ldS3cret", "REV": "{{commit}}"}}})
	put(AppSpec{Source: Source{Type: "git", Repo: "ssh://git@git.example.com/o/r.git", BuildArgs: map[string]string{"NPM_TOKEN": "npm_n3wS3cret"}}})
	id, _ := st.CreateDeploy(a.ID, old.ID, "t")
	st.FinishDeploy(id, "failed", "$ git clone ssh://git:hunter2pass@git.example.com/o/r.git\n$ docker build --build-arg NPM_TOKEN=npm_0ldS3cret --build-arg REV=0123456789abcdef .\nnpm_n3wS3cret\nERROR")
	e := &Engine{Store: st}
	ds, err := e.DeploySummaries(a.ID, 5, 4000)
	if err != nil || len(ds) != 1 {
		t.Fatal(ds, err)
	}
	full, err := e.DeployLog(a.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, log := range []string{ds[0].Log, full} {
		for _, secret := range []string{"hunter2pass", "npm_0ldS3cret", "npm_n3wS3cret"} {
			if strings.Contains(log, secret) {
				t.Errorf("read back %s:\n%s", secret, log)
			}
		}
		if !strings.Contains(log, "REV=0123456789abcdef") || !strings.Contains(log, "ERROR") {
			t.Errorf("redacted too much:\n%s", log)
		}
	}
}
