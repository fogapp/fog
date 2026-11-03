package build

import (
	"bytes"
	"io"
	"net/url"
	"strings"
	"sync"
)

// Redacted stands in for a secret in a build or fetch log.
const Redacted = "[redacted]"

// redactor is a writer that replaces each secret with Redacted before the
// bytes reach the deploy log — which the operator's page, notifications and
// MCP's fog_app_get all read. Lines are held until complete, so a secret
// split across two writes is still caught; Flush writes what is left.
type redactor struct {
	mu      sync.Mutex
	w       io.Writer
	secrets []string
	buf     []byte
}

func newRedactor(w io.Writer, secrets ...string) *redactor {
	r := &redactor{w: w}
	for _, s := range secrets {
		if len(s) >= 4 { // shorter would redact ordinary words
			r.secrets = append(r.secrets, s)
		}
	}
	return r
}

func (r *redactor) redact(s string) string {
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, Redacted)
	}
	return s
}

func (r *redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if i := bytes.LastIndexByte(r.buf, '\n'); i >= 0 {
		if _, err := io.WriteString(r.w, r.redact(string(r.buf[:i+1]))); err != nil {
			return 0, err
		}
		r.buf = append(r.buf[:0], r.buf[i+1:]...)
	}
	return len(p), nil
}

// Flush writes whatever is held back.
func (r *redactor) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) > 0 {
		io.WriteString(r.w, r.redact(string(r.buf)))
		r.buf = r.buf[:0]
	}
}

// repoSecrets are the parts of a repository URL that are credentials: a
// password in its userinfo (https://user:token@host/…), and the userinfo
// whole.
func repoSecrets(repo string) []string {
	u, err := url.Parse(repo)
	if err != nil || u.User == nil {
		return nil
	}
	var out []string
	if p, ok := u.User.Password(); ok && p != "" {
		out = append(out, p, url.QueryEscape(p), u.User.String())
	}
	return out
}

// redactRepo is repo, or any text naming it, with its credentials removed.
func redactRepo(s, repo string) string {
	return newRedactor(nil, repoSecrets(repo)...).redact(s)
}

// LogSecrets are the values a deploy log must never show for a source: its
// build-arg values (a registry token) and its repository credentials. A
// value naming {{commit}} is the commit, which is no secret.
func LogSecrets(buildArgs map[string]string, repo string) []string {
	var out []string
	for _, v := range buildArgs {
		if v != "" && !strings.Contains(v, "{{commit}}") {
			out = append(out, v)
		}
	}
	return append(out, repoSecrets(repo)...)
}

// RedactLog is log with every secret replaced by Redacted.
func RedactLog(log string, secrets []string) string {
	return newRedactor(nil, secrets...).redact(log)
}

// withoutPassword is repo with any password in its userinfo removed.
func withoutPassword(repo string) string {
	u, err := url.Parse(repo)
	if err != nil || u.User == nil {
		return repo
	}
	if _, has := u.User.Password(); !has {
		return repo
	}
	u.User = url.User(u.User.Username())
	return u.String()
}
