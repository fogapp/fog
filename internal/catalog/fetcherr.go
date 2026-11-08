package catalog

import (
	"net/url"
	"regexp"
	"strings"
)

// explainFetch says why a catalog couldn't be fetched, in the operator's
// terms, and what to change: the raw git line names a symptom ("could not
// read Username") that is several steps from the cause (a private or
// missing repository over https). An error no rule knows comes back as git
// said it.
func explainFetch(rm Remote, raw string) string {
	host := remoteHost(rm.URL)
	https := strings.HasPrefix(rm.URL, "https://")
	for _, rule := range fetchRules {
		if rule.https != nil && *rule.https != https {
			continue
		}
		if rule.match.MatchString(raw) {
			return strings.ReplaceAll(rule.say, "{host}", host)
		}
	}
	return raw
}

var yes, no = true, false

var fetchRules = []struct {
	match *regexp.Regexp
	https *bool // nil: either kind of URL
	say   string
}{
	// Before the https rule: a missing branch is "not found" too.
	{regexp.MustCompile(`(?i)remote branch .* not found|couldn't find remote ref|branch .* not found`), nil,
		"The repository has no such branch: check the branch name."},
	// GitHub, Gitea and the rest answer a missing or private repository
	// over https by asking for a password, which git is told never to do.
	{regexp.MustCompile(`(?i)could not read Username|terminal prompts disabled|authentication failed|repository not found|not found`), &yes,
		"{host} has no public repository at this URL. Over https fog reads only public catalogs: check the address, " +
			"or for a private catalog use its SSH URL and add this fog's catalog key (below) to it as a read-only deploy key."},
	{regexp.MustCompile(`(?i)failed to connect|connection refused|couldn't connect|ssl_|ssl certificate|tls handshake|certificate verify`), &yes,
		"{host} didn't answer over https. If it serves git over SSH only, or the catalog is private, use its SSH URL " +
			"(e.g. ssh://git@{host}/owner/repo.git) and add this fog's catalog key (below) to it as a read-only deploy key."},
	{regexp.MustCompile(`(?i)permission denied \(publickey|access denied|authentication failed|repository not found|does not appear to be a git repository`), &no,
		"{host} refused this fog's catalog key. Add the key below to the repository as a read-only deploy key, and check the SSH user " +
			"in the URL (Gitea's is usually gitea@, GitHub's git@)."},
	{regexp.MustCompile(`(?i)host key verification failed`), nil,
		"{host}'s SSH host key didn't match the one this fog has on record. If the git server was rebuilt, remove the catalog and add it again."},
	{regexp.MustCompile(`(?i)could not resolve host|name or service not known|no such host|nodename nor servname`), nil,
		"{host} can't be found from this fog: check the hostname in the URL."},
	{regexp.MustCompile(`(?i)connection refused|connection timed out|operation timed out|no route to host|network is unreachable|context deadline exceeded`), nil,
		"{host} didn't answer. If it's up, check that this fog can reach it."},
}

// remoteHost is the host a catalog URL names, for messages; the URL itself
// when it names none (a local path).
func remoteHost(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Hostname()
	}
	if m := scpLike.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return u
}

var scpLike = regexp.MustCompile(`^[\w.-]+@([\w.-]+):`)
