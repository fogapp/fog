package issues

import (
	"errors"

	"gitea.lan/matthew/fog/internal/store"
)

// ErrNoIssue is Mark's answer for an id that names no issue.
var ErrNoIssue = errors.New("no such issue")

// Mark applies an acknowledgement change (store.AckIssue, store.UnackIssue)
// to one issue and records it in the audit log under action, the same way
// whether the dashboard or an MCP client asked.
func Mark(st *store.Store, actor, action string, id int64, fn func(int64) error) error {
	issue, err := st.IssueByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNoIssue
	}
	if err != nil {
		return err
	}
	if err := fn(id); err != nil {
		return err
	}
	st.Audit(actor, action, issue.Kind, store.AuditDetail(map[string]any{"app": issue.AppName, "subject": issue.Subject}))
	return nil
}
