package server

import (
	"log/slog"

	"gitea.lan/matthew/fog/internal/appevents"
	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/config"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/feedback"
	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/filesfed"
	"gitea.lan/matthew/fog/internal/offsite"
	"gitea.lan/matthew/fog/internal/signing"
	"gitea.lan/matthew/fog/internal/store"
)

// newServerForTest is New as fogd calls it, for a test that names only the
// deps it cares about: every component main builds and the test left out
// is built over the same store and vault, with no loop running, and the
// server's hooks are installed as cmd/fogd/wire.go installs them.
func newServerForTest(cfg config.Config, st *store.Store, deps Deps, log *slog.Logger) *Server {
	if deps.Fed == nil {
		deps.Fed = &fed.Service{Store: st, Vault: deps.Vault, Log: log, Version: config.Version}
	}
	if deps.Flare != nil && deps.Flare.Fed == nil {
		deps.Flare.Fed = deps.Fed
	}
	if deps.Feedback == nil {
		// A report still tries its sends once, at the moment it is written.
		deps.Feedback = &feedback.Forwarder{Store: st, Fed: deps.Fed, Log: log}
	}
	if deps.Files == nil {
		// Its index opens lazily, so tests pay nothing.
		deps.Files = files.New(cfg.DataDir, st, log)
	}
	if deps.FilesFed == nil {
		deps.FilesFed = filesfed.New(deps.Files, deps.Fed, st, log)
	}
	if deps.Offsite == nil && deps.Vault != nil {
		deps.Offsite = &offsite.Manager{DataDir: cfg.DataDir, Store: st, Vault: deps.Vault, Log: log,
			Passphrase: func() string { v, _ := st.GetSetting(backup.SettingRecoveryKitPassphrase); return v },
			ServerName: func() string { v, _ := st.GetSetting(store.SettingServerName); return v }}
	}
	if deps.Events == nil {
		deps.Events = appevents.New(st)
	}
	if deps.Keys == nil {
		deps.Keys = &signing.Keyring{Store: st, Vault: deps.Vault}
	}
	if deps.Chat == nil {
		deps.Chat = NewChatService(cfg, st, deps.Fed, log)
	}
	srv := New(cfg, st, deps, log)
	hooks := srv.Hooks()
	deps.Fed.PeopleFor = deps.Chat.Fed.PeopleFor
	deps.Fed.FeedbackFor = hooks.FeedbackFor
	deps.Fed.OnFeedback = hooks.OnFeedback
	deps.FilesFed.AppNotify = hooks.FilesChanged
	if deps.Chat.Notify == nil {
		deps.Chat.Notify = &chat.Notifier{Svc: deps.Chat, Vault: deps.Vault, NameFor: hooks.DisplayName}
	}
	return srv
}
