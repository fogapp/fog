package apps

import (
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/store"
)

func versionEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Engine{Store: st}
}

// A deploy of the branch head is up to date by definition. A rollback is
// not: it deliberately runs something older and must keep saying so, or the
// badge would clear and hide the very update just stepped back from until
// the next scheduled check hours later.
func TestSetDeployedCommitUpdate(t *testing.T) {
	const head = "fedcba9876543210fedcba9876543210fedcba98"
	const older = "0123456789abcdef0123456789abcdef01234567"

	t.Run("deploying the head clears the badge", func(t *testing.T) {
		e := versionEngine(t)
		app, _ := e.Store.CreateApp("manor")
		if err := e.setDeployedCommitUpdate(app.ID, head, false); err != nil {
			t.Fatal(err)
		}
		u, err := e.Store.AppUpdateFor(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if u.Available || u.LocalDigest != head || u.RemoteDigest != head {
			t.Errorf("after deploying the head: %+v", u)
		}
	})

	t.Run("rolling back keeps the badge up", func(t *testing.T) {
		e := versionEngine(t)
		app, _ := e.Store.CreateApp("manor")
		if err := e.Store.SetAppUpdate(app.ID, head, head, false); err != nil {
			t.Fatal(err)
		}
		if err := e.setDeployedCommitUpdate(app.ID, older, true); err != nil {
			t.Fatal(err)
		}
		u, err := e.Store.AppUpdateFor(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !u.Available {
			t.Error("an app rolled back to an older commit reads as up to date")
		}
		if u.LocalDigest != older || u.RemoteDigest != head {
			t.Errorf("rolled back state: %+v", u)
		}
	})

	t.Run("rolling back with no known head claims nothing", func(t *testing.T) {
		e := versionEngine(t)
		app, _ := e.Store.CreateApp("manor")
		if err := e.setDeployedCommitUpdate(app.ID, older, true); err != nil {
			t.Fatal(err)
		}
		u, err := e.Store.AppUpdateFor(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Nothing has ever been compared, so there is no remote to be
		// behind. Inventing one either way would be a claim fog can't make.
		if u.Available {
			t.Errorf("claimed an update with no remote ever checked: %+v", u)
		}
	})
}

// DeployVersion is the rollback door; without a version it is a redeploy
// wearing the wrong name, and must say so rather than quietly deploying
// the head.
func TestDeployVersionNeedsAVersion(t *testing.T) {
	e := versionEngine(t)
	if err := e.DeployVersion(t.Context(), "manor", "", "matthew"); err == nil {
		t.Fatal("DeployVersion accepted an empty version")
	}
}
