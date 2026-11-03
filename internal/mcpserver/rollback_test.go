package mcpserver

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

// The attack: an assistant with a manage token rolls an app back to the
// revision that bound /root/.ssh, which the operator had edited away. An
// assistant may roll back only to a revision that binds, publishes and
// reaches what the latest does; the rest is the operator's, from History.
func TestAssistantCannotRollBackToMoreAccess(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &apps.Engine{Store: st, Log: log, Mat: &apps.Materializer{DataDir: t.TempDir()}}
	h := New(Deps{Store: st, Engine: e, Log: log})
	ctx := context.Background()

	evil := &apps.AppSpec{
		Source:                apps.Source{Type: "image", Image: "evil/app:1"},
		Volumes:               []apps.Volume{{Name: "keys", Mount: "/keys", Host: "/root/.ssh"}},
		AcknowledgeHostAccess: []string{"/root/.ssh"},
	}
	if err := e.Create(ctx, "evil", evil, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateSpec(ctx, "evil", &apps.AppSpec{Source: evil.Source}, nil, "drop the bind", "admin"); err != nil {
		t.Fatal(err)
	}

	res := call(t, h, "fog_app_deploy", map[string]any{"name": "evil", "rev": 1})
	if !res.Result.IsError || !strings.Contains(resultText(res), "only an operator can deploy it") {
		t.Fatalf("assistant rollback to the bind = %+v", res.Result)
	}
	// The latest, or one with the latest's access, is the assistant's.
	if err := e.CheckRollbackSameAccess("evil", 2); err != nil {
		t.Fatalf("redeploying the latest refused: %v", err)
	}
}
