// Telling a person what happened to their own sign-in (docs/signin-contract.md).
// Changes to someone's credentials and sessions were audited and nothing
// more; now the person hears of each one — through the channels they set up
// to be reached (Web Push to their devices, their ntfy topic) — and
// Settings › Your sign-in lists them, so a change they didn't make is one
// they can see and undo.
//
// The hook is the audit log itself (auditAs calls tellAccountChange): any
// path that records a credential or session change tells the person, so a
// new path cannot forget to.
package server

import (
	"encoding/json"

	"gitea.lan/matthew/fog/internal/store"
)

// accountActions are the audit actions that change how a person signs in.
// Each has the person's handle as its subject.
var accountActions = map[string]bool{
	"auth.login":               true, // a password sign-in
	"people.login":             true, // a passkey sign-in
	"people.password.set":      true,
	"people.passkey.add":       true,
	"people.credential.remove": true,
	"people.session.revoke":    true,
	"people.signout":           true,
	"people.identity.reset":    true,
}

// accountSettingsPath is where a person reviews their sign-in.
const accountSettingsPath = "/settings/access/signin"

// accountEvent is one change to a person's sign-in, as they are shown it.
type accountEvent struct {
	At      string `json:"at"`
	Action  string `json:"action"`
	Summary string `json:"summary"`
	// ByOther: someone other than the person did it (the operator).
	ByOther bool `json:"byOther"`
}

// accountSummary says what an action did, to the person it was done to.
// byOther is whether someone else did it.
func accountSummary(action string, detail map[string]any, byOther bool) string {
	switch action {
	case "auth.login":
		return "Signed in with your password"
	case "people.login":
		return "Signed in with a passkey"
	case "people.password.set":
		if byOther {
			return "The person who runs fog set a new password for you, and signed you out everywhere"
		}
		return "Your password was changed, and your other sessions were signed out"
	case "people.passkey.add":
		return "A passkey was added"
	case "people.credential.remove":
		if detail["kind"] == store.CredentialPassword {
			return "Your password was removed"
		}
		return "A passkey was removed"
	case "people.session.revoke":
		return "A session was signed out"
	case "people.signout":
		return "The person who runs fog signed you out everywhere"
	case "people.identity.reset":
		return "The person who runs fog reset your chat identity and signed out your devices"
	}
	return action
}

// tellAccountChange is auditAs's hook: when an entry records a change to
// someone's sign-in, that person is told, off the request.
func (s *Server) tellAccountChange(actor, action, subject string, detail map[string]any) {
	if !accountActions[action] || subject == "" || s.chat == nil || s.chat.Notify == nil {
		return
	}
	notifier := s.chat.Notify
	s.goSafe("account notice", func() {
		p, err := s.store.PersonByHandle(subject)
		if err != nil || p.UID == "" {
			return
		}
		summary := accountSummary(action, detail, actor != subject)
		notifier.NotifyAccount(p.UID, "Your fog sign-in: "+summary,
			"If this wasn't you, change your password and sign out the sessions you don't know in Settings › Your sign-in.",
			accountSettingsPath)
	})
}

// accountActivity is a person's recent sign-in changes, newest first.
func (s *Server) accountActivity(p *store.Person) ([]accountEvent, error) {
	actions := make([]string, 0, len(accountActions))
	for a := range accountActions {
		actions = append(actions, a)
	}
	entries, err := s.store.AccountActivity(p.Handle, actions, 20)
	if err != nil {
		return nil, err
	}
	out := make([]accountEvent, 0, len(entries))
	for _, e := range entries {
		var detail map[string]any
		json.Unmarshal([]byte(e.Detail), &detail)
		byOther := e.Actor != p.Handle
		out = append(out, accountEvent{At: e.At, Action: e.Action, Summary: accountSummary(e.Action, detail, byOther), ByOther: byOther})
	}
	return out, nil
}
