package proxy

import (
	"context"
	"errors"
	"gitea.lan/matthew/fog/internal/vault"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/store"
)

func TestCaddyfileWildcard(t *testing.T) {
	p := &Proxy{}
	routes := []route{
		{Host: "fog.home.example.com", Upstream: "host.docker.internal:8080", Fog: true},
		{Host: "sonarr.home.example.com", Upstream: "sonarr:8989"},
		{Host: "hub.home.example.com", Upstream: "hub:80", NoBuffer: true},
	}
	out := p.caddyfile("home.example.com", routes, true, "")
	for _, want := range []string{
		"*.home.example.com, home.example.com {",
		"dns cloudflare {env.DNS_API_TOKEN}",
		matcherName("sonarr.home.example.com") + " host sonarr.home.example.com",
		"reverse_proxy sonarr:8989",
		matcherName("hub.home.example.com") + " host hub.home.example.com",
		"flush_interval -1",
		"header_up X-Fog-Client-Ip {remote_host}",
		"header_up X-Fog-Proxy-Secret {env.FOG_PROXY_SECRET}",
		"respond \"unknown app\" 404",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("caddyfile missing %q\n%s", want, out)
		}
	}
}

// Only fogd's own route carries the client address and forward secret; an
// app must never see the secret.
func TestCaddyfileForwardSecretOnlyToFog(t *testing.T) {
	p := &Proxy{}
	routes := []route{{Host: "sonarr.home.example.com", Upstream: "sonarr:8989"}}
	for _, dns := range []bool{true, false} {
		if out := p.caddyfile("home.example.com", routes, dns, ""); strings.Contains(out, HeaderSecret) {
			t.Errorf("app route carries the forward secret:\n%s", out)
		}
	}
}

// A custom route host can share its first label with another app's default
// host. Both must still get their own matcher, or the second block is dead.
func TestCaddyfileMatchersAreUniquePerHost(t *testing.T) {
	p := &Proxy{}
	routes := []route{
		{Host: "claw.home.example.com", Upstream: "openclaw:8080"},
		{Host: "claw.apps.home.example.com", Upstream: "other:8080"},
	}
	out := p.caddyfile("home.example.com", routes, true, "")
	for _, want := range []string{
		matcherName("claw.home.example.com") + " host claw.home.example.com",
		matcherName("claw.apps.home.example.com") + " host claw.apps.home.example.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("caddyfile missing %q\n%s", want, out)
		}
	}
}

func TestComposeJoinsTheSealedNetwork(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	raw, err := (&Proxy{Store: st}).composeFile()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Networks map[string]any `yaml:"networks"`
		} `yaml:"services"`
		Networks map[string]struct {
			External bool `yaml:"external"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{apps.NetworkName, apps.SealedNetworkName} {
		if _, ok := doc.Services["caddy"].Networks[n]; !ok || !doc.Networks[n].External {
			t.Errorf("caddy isn't on %s:\n%s", n, raw)
		}
	}
}

// lanBranch splits a rendered lanOnly route into what the allowed branch
// holds and what is left outside it, by the branch's closing brace.
func lanBranch(t *testing.T, conf, host, indent string) (inside, outside string) {
	t.Helper()
	open := indent + "handle @lan_" + host + " {\n"
	start := strings.Index(conf, open)
	if start < 0 {
		t.Fatalf("no allowed branch for %s:\n%s", host, conf)
	}
	rest := conf[start+len(open):]
	end := strings.Index(rest, "\n"+indent+"}\n")
	if end < 0 {
		t.Fatalf("branch never closes:\n%s", conf)
	}
	return rest[:end], conf[:start] + rest[end:]
}

func lanRoutes() []route {
	return []route{
		{Host: "fog.example.com", Upstream: "host.docker.internal:8080", Fog: true},
		{Host: "fit.example.com", Upstream: "fit:3000", LanOnly: true},
		{Host: "gated.example.com", Upstream: "gated:80", LanOnly: true, Auth: true},
		{Host: "sonarr.example.com", Upstream: "sonarr:8989"},
	}
}

// Both shapes of Caddyfile, wildcard and plain HTTP, guard a lanOnly route
// the same way, and with a refusal that isn't the app's.
func TestCaddyfileLanOnly(t *testing.T) {
	p := &Proxy{FogUpstream: "host.docker.internal:8080"}
	for name, tc := range map[string]struct {
		hasDNS bool
		indent string
	}{"wildcard": {true, "\t\t"}, "http": {false, "\t"}} {
		t.Run(name, func(t *testing.T) {
			conf := p.caddyfile("example.com", lanRoutes(), tc.hasDNS, "")
			for _, want := range []string{
				tc.indent + "@lan_" + strings.TrimPrefix(matcherName("fit.example.com"), "@") + " {\n" + tc.indent + "\tremote_ip private_ranges\n" + tc.indent + "\tnot header Cf-Connecting-Ip *\n" + tc.indent + "}\n",
				tc.indent + "handle {\n" + tc.indent + "\trespond \"" + lanRefusal + "\" 403\n" + tc.indent + "}\n",
			} {
				if !strings.Contains(conf, want) {
					t.Errorf("missing:\n%s\nin:\n%s", want, conf)
				}
			}
			inside, outside := lanBranch(t, conf, strings.TrimPrefix(matcherName("fit.example.com"), "@"), tc.indent)
			if !strings.Contains(inside, "reverse_proxy fit:3000") {
				t.Errorf("the app is outside the allowed branch:\n%s", conf)
			}
			if strings.Contains(outside, "reverse_proxy fit:3000") {
				t.Errorf("the app is reachable outside the allowed branch:\n%s", conf)
			}
		})
	}
}

// Every way into a gated lanOnly route is behind the check, the gate's own
// callback included: Caddy sorts a handle with a path first, so one left
// beside the check would run ahead of it.
func TestCaddyfileLanOnlyCoversTheGate(t *testing.T) {
	p := &Proxy{FogUpstream: "host.docker.internal:8080"}
	conf := p.caddyfile("example.com", lanRoutes(), true, "")
	inside, outside := lanBranch(t, conf, strings.TrimPrefix(matcherName("gated.example.com"), "@"), "\t\t")
	for _, want := range []string{"handle /.fog/gate/* {", "forward_auth host.docker.internal:8080 {", "reverse_proxy gated:80"} {
		if !strings.Contains(inside, want) {
			t.Errorf("%q is outside the allowed branch:\n%s", want, conf)
		}
	}
	if strings.Count(conf, "/.fog/gate/*") != 1 || strings.Contains(outside, "reverse_proxy gated:80") {
		t.Errorf("a way around the branch:\n%s", conf)
	}
}

// The check reads the TCP peer. remote_ip with "forwarded", or client_ip,
// would take the client's word in X-Forwarded-For.
func TestCaddyfileLanOnlyTrustsOnlyThePeer(t *testing.T) {
	p := &Proxy{FogUpstream: "host.docker.internal:8080"}
	conf := p.caddyfile("example.com", lanRoutes(), true, "")
	for _, bad := range []string{"forwarded", "client_ip", "trusted_proxies"} {
		if strings.Contains(conf, bad) {
			t.Errorf("found %q:\n%s", bad, conf)
		}
	}
}

func TestCaddyfileOtherRoutesAreUntouched(t *testing.T) {
	p := &Proxy{FogUpstream: "host.docker.internal:8080"}
	routes := lanRoutes()
	conf := p.caddyfile("example.com", []route{routes[0], routes[3]}, true, "")
	for _, bad := range []string{"remote_ip", "403", "@lan_"} {
		if strings.Contains(conf, bad) {
			t.Errorf("found %q on a public route:\n%s", bad, conf)
		}
	}
}

// The field reaches the Caddyfile from the spec, not only from a hand-built
// route.
func TestCollectRoutesCarriesLanOnly(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app, err := st.CreateApp("fit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRevision(app.ID, `{"source":{"type":"image","image":"x:1"},"routes":[{"port":3000,"lanOnly":true}]}`, "", "t"); err != nil {
		t.Fatal(err)
	}
	routes, err := (&Proxy{Store: st, FogUpstream: "h:1"}).collectRoutes("example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Host == "fit.example.com" && !r.LanOnly {
			t.Errorf("lost lanOnly: %+v", r)
		}
		if r.Fog && r.LanOnly {
			t.Errorf("fog's own route went LAN-only: %+v", r)
		}
	}
}

func TestCaddyfileHTTPFallback(t *testing.T) {
	p := &Proxy{}
	routes := []route{{Host: "sonarr.home.example.com", Upstream: "sonarr:8989"}}
	out := p.caddyfile("home.example.com", routes, false, "")
	if !strings.Contains(out, "http://sonarr.home.example.com {") {
		t.Errorf("expected plain-http site block:\n%s", out)
	}
	if strings.Contains(out, "dns cloudflare") {
		t.Error("should not configure DNS-01 without a token")
	}
}

// One app's bad state must not cost every other app its routes: an app
// whose stored spec cannot be loaded is left out (the issues sweep files
// it, by the same rule), and the rest still serve. An app with no revisions
// yet is legitimately routeless and skipped.
func TestCollectRoutesSpecErrors(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := &Proxy{Store: st}

	// No revisions yet: skipped, not fatal.
	if _, err := st.CreateApp("newborn"); err != nil {
		t.Fatal(err)
	}
	routes, err := p.collectRoutes("home.example.com")
	if err != nil {
		t.Fatalf("app without revisions must not fail collectRoutes: %v", err)
	}
	if len(routes) != 1 { // just the fog route
		t.Fatalf("want only the fog route, got %d routes", len(routes))
	}

	// A corrupt spec beside a healthy app: the healthy one keeps its route.
	app, err := st.CreateApp("broken")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRevision(app.ID, `{not json`, "", "t"); err != nil {
		t.Fatal(err)
	}
	ok, err := st.CreateApp("healthy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRevision(ok.ID, `{"source":{"type":"image","image":"x"},"routes":[{"port":80}]}`, "", "t"); err != nil {
		t.Fatal(err)
	}
	routes, err = p.collectRoutes("home.example.com")
	if err != nil {
		t.Fatalf("one corrupt spec failed every route: %v", err)
	}
	hosts := map[string]bool{}
	for _, r := range routes {
		hosts[r.Host] = true
	}
	if !hosts["healthy.home.example.com"] || hosts["broken.home.example.com"] {
		t.Fatalf("routes = %v, want healthy routed and broken left out", hosts)
	}
}

// A gated route scrubs the identity headers a client could forge, hands
// /.fog/gate/* to fogd, asks fogd before every request, and never forwards
// fog's cookies to the app.
func TestCaddyfileGatedRoute(t *testing.T) {
	p := &Proxy{FogUpstream: "fogd:8080"}
	routes := []route{{Host: "sab.home.example.com", Upstream: "sabnzbd:8080", Auth: true}}
	for _, dns := range []bool{true, false} {
		out := p.caddyfile("home.example.com", routes, dns, "")
		for _, want := range []string{
			"request_header -Remote*",
			"request_header -Fog*",
			"handle /.fog/gate/* {",
			"forward_auth fogd:8080 {",
			"uri /api/authgate",
			"copy_headers Remote-User Remote-Name Remote-Groups Remote-Email Fog-Assertion",
			"reverse_proxy sabnzbd:8080 {",
			stripFogCookies,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("dns=%v: caddyfile missing %q\n%s", dns, want, out)
			}
		}
		// The scrub must come before the gate: Caddy orders request_header
		// ahead of handle, and forward_auth sits inside a nested handle.
		if strings.Index(out, "request_header -Remote*") > strings.Index(out, "forward_auth") {
			t.Errorf("identity headers scrubbed after the gate runs:\n%s", out)
		}
	}
}

// Every app route drops fog's cookies, gated or not: browsers still carry
// the domain-wide session cookie fog issued before it went host-only.
func TestCaddyfileStripsFogCookiesFromEveryApp(t *testing.T) {
	p := &Proxy{}
	routes := []route{{Host: "hub.home.example.com", Upstream: "hub:80"}}
	out := p.caddyfile("home.example.com", routes, true, "")
	if !strings.Contains(out, stripFogCookies) {
		t.Errorf("app route forwards fog's cookies:\n%s", out)
	}
	if !strings.Contains(out, "request_header -Remote*") {
		t.Errorf("ungated app route passes a client's Remote-User through:\n%s", out)
	}
	re := regexp.MustCompile(`\b(?:__Host-)?fog_(?:gate|session|person)=[^;]*(?:;\s*)?`)
	for in, want := range map[string]string{
		"__Host-fog_gate=abc; theme=dark":         "theme=dark",
		"theme=dark; fog_session=abc":             "theme=dark; ",
		"a=1; fog_session=x; b=2":                 "a=1; b=2",
		"my_fog_session=keep":                     "my_fog_session=keep",
		"__Host-fog_session=s; __Host-fog_gate=g": "",
	} {
		if got := re.ReplaceAllString(in, ""); got != want {
			t.Errorf("strip(%q) = %q, want %q", in, got, want)
		}
	}
}

// The attack: specs stored behind the save check's back (before it existed,
// or a domain change made two names one) claim fog's hostname, another
// app's, and a name that isn't one. Caddy refuses a config with two blocks
// for one name, so one such spec used to cost every app its routes. Now
// fog's name stays fog's, the oldest claimant keeps a shared one, and the
// rest are left out — the config renders with one block per name.
func TestCollectRoutesSkipsClashesInsteadOfFailing(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	put := func(name, spec string) {
		a, err := st.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateRevision(a.ID, spec, "", "t"); err != nil {
			t.Fatal(err)
		}
	}
	put("owner", `{"source":{"type":"image","image":"x"},"routes":[{"host":"shared","port":80}]}`)
	put("thief", `{"source":{"type":"image","image":"x"},"routes":[{"host":"shared","port":81},{"port":82}]}`)
	put("console", `{"source":{"type":"image","image":"x"},"routes":[{"host":"fog","port":80}]}`)
	put("inject", `{"source":{"type":"image","image":"x"},"routes":[{"host":"a {\n}\nhttp://evil","port":80}]}`)
	p := &Proxy{Store: st, FogUpstream: "fogd:8080"}
	routes, err := p.collectRoutes("home.example.com")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range routes {
		if _, dup := got[r.Host]; dup {
			t.Fatalf("two routes for %s", r.Host)
		}
		got[r.Host] = r.Upstream
	}
	want := map[string]string{
		"fog.home.example.com":    "fogd:8080",
		"shared.home.example.com": "owner.fog-app:80",
		"thief.home.example.com":  "thief.fog-app:82", // its other route still serves
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	for h, up := range want {
		if got[h] != up {
			t.Errorf("%s → %q, want %q", h, got[h], up)
		}
	}
	cfg := p.caddyfile("home.example.com", routes, true, "")
	if strings.Contains(cfg, "evil") || strings.Count(cfg, "host fog.home.example.com\n") != 1 {
		t.Errorf("config:\n%s", cfg)
	}
}

// An ACME email that would be Caddyfile syntax of its own is dropped, not
// written.
func TestApplyDropsInjectedAcmeEmail(t *testing.T) {
	if acmeEmailRe.MatchString("ops@example.com\n}\nhttp://evil {") || !acmeEmailRe.MatchString("ops+fog@example.com") {
		t.Fatal("acme email check is wrong")
	}
}

// The attack: a public IPv6 client reaching a lanOnly route. Published on
// [::], Docker's userland proxy handed Caddy the bridge gateway as the
// peer, which private_ranges admits. Caddy's ports are IPv4 only.
func TestCaddyPublishesIPv4Only(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	out, err := (&Proxy{Store: st}).composeFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"0.0.0.0:80:80", "0.0.0.0:443:443", "0.0.0.0:443:443/udp"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("compose missing %s:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if l == "80:80" || l == "443:443" || l == "443:443/udp" || strings.HasPrefix(l, "[::]") {
			t.Errorf("published on every address family: %q", l)
		}
	}
}

// The attack: x-y.example.com and x.y.example.com once made the same
// matcher, so one host's block answered for the other. Each host's matcher
// (and its LAN matcher) is its own.
func TestMatcherNamesNeverCollide(t *testing.T) {
	hosts := []string{"x-y.example.com", "x.y.example.com", "x_y.example.com", "xy.example.com", "x--y.example.com"}
	seen := map[string]string{}
	for _, h := range hosts {
		m := matcherName(h)
		if other, dup := seen[m]; dup {
			t.Fatalf("%s and %s share matcher %s", h, other, m)
		}
		seen[m] = h
	}
	p := &Proxy{FogUpstream: "fogd:8080"}
	cfg := p.caddyfile("example.com", []route{
		{Host: "x-y.example.com", Upstream: "a.fog-app:80", LanOnly: true},
		{Host: "x.y.example.com", Upstream: "b.fog-app:80", LanOnly: true},
	}, true, "")
	if strings.Count(cfg, matcherName("x-y.example.com")+" host x-y.example.com") != 1 ||
		strings.Count(cfg, matcherName("x.y.example.com")+" host x.y.example.com") != 1 {
		t.Fatalf("config:\n%s", cfg)
	}
}

// A config Caddy rejects is never left where a restart would load it: the
// live file keeps the last accepted one, and the rejected one goes.
func TestRejectedConfigIsNotLeftLive(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vlt, _ := vault.Open(dir)
	st.SetSetting(store.SettingBaseDomain, "home.example.com")
	reject := false
	p := &Proxy{DataDir: dir, Store: st, Vault: vlt, FogUpstream: "fogd:8080",
		converge: func(context.Context, string) error {
			if reject {
				return errors.New("generated Caddyfile rejected")
			}
			return nil
		},
		caddyRunning: func(context.Context) bool { return false },
		wireConverge: func(context.Context, string) error { return nil },
		subnets:      noSubnets,
	}
	if err := p.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "caddy", "conf", "Caddyfile")
	good, _ := os.ReadFile(live)
	reject = true
	st.SetSetting(SettingAcmeEmail, "ops@example.com") // changes the config
	if err := p.Apply(t.Context()); err == nil {
		t.Fatal("a rejected config applied")
	}
	if now, _ := os.ReadFile(live); string(now) != string(good) {
		t.Fatalf("the live config changed to the rejected one:\n%s", now)
	}
	if _, err := os.Stat(filepath.Join(dir, "caddy", "conf", stagedName)); err == nil {
		t.Fatal("the rejected config was left staged")
	}
}
