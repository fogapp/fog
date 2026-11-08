package main

import (
	"context"
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/server"
	"gitea.lan/matthew/fog/internal/store/storetest"
	"gitea.lan/matthew/fog/internal/vault"
)

// server.New installs none of its hooks; the wire steps do. A hook left out
// here fails quietly in production — peers never hear of our people, an app
// never hears its mounted folder changed — so each one is checked.
func TestWireInstallsTheServerHooks(t *testing.T) {
	dir := t.TempDir()
	st, err := storetest.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vlt, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &daemon{cfg: config.Config{DataDir: dir, ListenAddr: "127.0.0.1:0"}, log: quietLog(), ctx: ctx, st: st, vlt: vlt}
	d.tasks.log = d.log

	// The steps in run's order.
	d.wireApps()
	d.wireNetworking()
	d.wireFederation()
	d.wireFiles()
	defer d.fm.Close()
	d.wireOffsite()
	d.wireIssues()
	d.wireHooks()
	d.wirePGUpgrader()
	d.chatSvc = server.NewChatService(d.cfg, d.st, d.fedSvc, d.log)
	d.wireHTTP()

	for name, set := range map[string]bool{
		"fed.DirectoryFor":     d.fedSvc.DirectoryFor != nil,
		"fed.PeopleFor":        d.fedSvc.PeopleFor != nil,
		"fed.FeedbackFor":      d.fedSvc.FeedbackFor != nil,
		"fed.OnFeedback":       d.fedSvc.OnFeedback != nil,
		"flare.Fed":            d.flareFwd.Fed == d.fedSvc,
		"feedback.Fed":         d.feedbackFwd.Fed == d.fedSvc,
		"filesfed.AppNotify":   d.ffed.AppNotify != nil,
		"engine.OnSpecChanged": d.engine.OnSpecChanged != nil,
		"materializer.Policy":  d.engine.Mat.MountPolicy != nil,
		"chat.Notify":          d.chatSvc.Notify != nil,
		"chat.Notify.NameFor":  d.chatSvc.Notify != nil && d.chatSvc.Notify.NameFor != nil,
		"chat.Notify.Ntfy":     d.chatSvc.Notify != nil && d.chatSvc.Notify.Ntfy != nil,
	} {
		if !set {
			t.Errorf("%s is not wired", name)
		}
	}
}
