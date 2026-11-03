package apps

import (
	"regexp"
	"strings"
)

// Diagnosis turns a raw failure log into a plain-English headline and fix.
// The operator should never need to read git/docker output to know what to
// do next; the raw log stays one click away for the rare unmatched case.
type Diagnosis struct {
	Title string `json:"title"`
	Fix   string `json:"fix"`
}

type diagRule struct {
	match *regexp.Regexp
	diag  Diagnosis
}

var diagRules = []diagRule{
	// Must precede the generic auth rule: http(s) URLs fail differently.
	{regexp.MustCompile(`(?i)authentication failed for 'http|could not read Username for 'http`), Diagnosis{
		Title: "This is an HTTP clone URL, which needs a password",
		Fix:   "Deploy keys work over SSH only. Use the SSH form of the URL (for this Gitea: ssh://gitea@gitea.lan/<owner>/<repo>.git), not the http:// one from the clone box.",
	}},
	{regexp.MustCompile(`(?i)permission denied \(publickey|access denied|authentication failed`), Diagnosis{
		Title: "The git server rejected the connection",
		Fix: "If you haven't yet, add the deploy key shown above to the repository (Settings → Deploy keys in your git host). " +
			"If the key IS added, the SSH user in the repository URL is probably wrong — Gitea often uses gitea@ rather than git@ (check the SSH clone URL on the repo page).",
	}},
	{regexp.MustCompile(`(?i)host key verification failed`), Diagnosis{
		Title: "The git server's host key was rejected",
		Fix:   "The server's SSH identity changed or couldn't be verified. If you recently rebuilt your git server, delete the app's build cache and retry.",
	}},
	{regexp.MustCompile(`(?i)could not resolve host|name or service not known|no such host`), Diagnosis{
		Title: "The repository's server can't be found",
		Fix:   "Check the repository URL's hostname — it doesn't resolve from this server.",
	}},
	{regexp.MustCompile(`(?i)repository not found|does not appear to be a git repository|fatal: .*not found`), Diagnosis{
		Title: "No repository at that URL",
		Fix:   "Check the repository URL path (owner/name), and that the deploy key has access to it.",
	}},
	{regexp.MustCompile(`(?i)branch .* not found on the remote`), Diagnosis{
		Title: "The branch doesn't exist",
		Fix:   "Check the branch name in Config — the repository doesn't have it.",
	}},
	{regexp.MustCompile(`(?i)no dockerfile at`), Diagnosis{
		Title: "No Dockerfile in the repository",
		Fix:   "The repository was cloned fine but has no Dockerfile at the configured path. Set the correct path in Config → Dockerfile.",
	}},
	{regexp.MustCompile(`(?i)docker build failed|failed to solve|error: failed to build`), Diagnosis{
		Title: "The image build failed",
		Fix:   "The code cloned fine but docker build errored — the failing step is near the bottom of the log.",
	}},
	{regexp.MustCompile(`(?i)NAS share .* wouldn't mount`), Diagnosis{
		Title: "A NAS share this app needs couldn't be mounted",
		Fix: "One of the app's volumes lives on a network share that didn't mount. Check the share in " +
			"Settings → Storage → Disks & shares — is the NAS powered on and reachable from this server?",
	}},
	{regexp.MustCompile(`(?i)release command failed`), Diagnosis{
		Title: "The release step (usually migrations) failed",
		Fix:   "The image built, but the release command errored — often a database migration. Details are at the bottom of the log.",
	}},
	{regexp.MustCompile(`(?i)never became healthy`), Diagnosis{
		Title: "The app started but never answered its health check",
		Fix:   "The container is running but the health endpoint didn't respond in time. Check the app's Logs tab for startup errors; the previous version was kept running.",
	}},
	{regexp.MustCompile(`(?i)postgres provisioning`), Diagnosis{
		Title: "Database provisioning failed",
		Fix:   "Fog couldn't prepare the managed Postgres for this app. Check that Docker has capacity and see the log for the exact error.",
	}},
	{regexp.MustCompile(`(?i)cannot connect to the docker daemon|docker daemon is not running`), Diagnosis{
		Title: "Docker isn't reachable",
		Fix:   "The Docker service appears to be down on this server. Check Settings → Server → Docker.",
	}},
	{regexp.MustCompile(`(?i)no space left on device`), Diagnosis{
		Title: "The server is out of disk space",
		Fix:   "Free some space (old images and builds are common culprits), then retry the deploy.",
	}},
	{regexp.MustCompile(`(?i)secret .*: not found`), Diagnosis{
		Title: "A required secret has no value",
		Fix:   "One of the app's declared secrets was never given a value. Open Config → Secrets and fill it in.",
	}},
}

// Diagnose matches known failure shapes in a log. Returns nil when nothing
// matches — the UI then shows the raw log directly.
func Diagnose(log string) *Diagnosis {
	for _, r := range diagRules {
		if r.match.MatchString(log) {
			d := r.diag
			return &d
		}
	}
	return nil
}

// LogTail returns the last n characters, starting at a line boundary.
func LogTail(log string, n int) string {
	if len(log) <= n {
		return log
	}
	tail := log[len(log)-n:]
	// Only trim a partial first line — if the cut landed on a boundary the
	// tail is already clean.
	if log[len(log)-n-1] != '\n' {
		if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
			tail = tail[i+1:]
		}
	}
	return tail
}
