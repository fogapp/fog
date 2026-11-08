package catalog

import (
	"strings"
	"testing"
)

// The lines git prints for the usual mistakes are put in the operator's
// terms, and anything unrecognised is passed through as git said it.
func TestExplainFetch(t *testing.T) {
	gh := Remote{Name: "fog-apps", URL: "https://github.com/fogapp/catalog.git"}
	lan := Remote{Name: "private", URL: "https://git.home.example/me/private-catalog"}
	ssh := Remote{Name: "private", URL: "ssh://gitea@git.home.example/me/private-catalog.git"}
	for _, c := range []struct {
		rm   Remote
		raw  string
		want string
	}{
		{gh, "git clone failed: fatal: could not read Username for 'https://github.com': terminal prompts disabled", "github.com has no public repository"},
		{lan, "git clone failed: fatal: unable to access 'https://git.home.example/me/private-catalog/': Failed to connect to git.home.example port 443 after 1 ms: Couldn't connect to server", "use its SSH URL (e.g. ssh://git@git.home.example/"},
		{ssh, "git clone failed: gitea@git.home.example: Permission denied (publickey).", "git.home.example refused this fog's catalog key"},
		{ssh, "git clone failed: ssh: Could not resolve hostname git.home.example: nodename nor servname provided", "git.home.example can't be found"},
		{gh, "git clone failed: warning: Could not find remote branch dev to clone.\nfatal: Remote branch dev not found in upstream origin", "no such branch"},
		{gh, "something new", "something new"},
		// What fetch makes of ssh's whole answer, advice and all.
		{ssh, "git clone failed: " + gitSaid("Cloning into '/x'...\ngitea@git.home.example: Permission denied (publickey,password).\r\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n"), "git.home.example refused this fog's catalog key"},
	} {
		if got := explainFetch(c.rm, c.raw); !strings.Contains(got, c.want) {
			t.Errorf("explainFetch(%s, %q) = %q, want it to contain %q", c.rm.URL, c.raw, got, c.want)
		}
	}
}
