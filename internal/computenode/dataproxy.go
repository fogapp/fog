package computenode

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

// A run's ffmpeg reaches the network only through its data proxy, on
// loopback. Its URLs name only the app's path (http://127.0.0.1:<port>/
// fog/compute/…): the proxy, one per run, knows the run's data route on
// fog and puts the app's path under it, MACed as this machine (DataMAC),
// so the run's token appears in no process's arguments. It forwards reads
// under /fog/compute/ and nothing else: no other path, no other host, no
// writes (outputs are uploaded by fog-node itself).
//
// It serves only the run's ffmpeg. On Linux, confined, it listens in the
// run's own network namespace, where nothing else is; on a Mac, or
// unconfined, its port is on the host's loopback, open to every account
// on the machine, and ffmpeg's arguments (which any account can read) say
// where. So ffmpeg uses it as its HTTP proxy, from http_proxy in its
// environment (which only its own user can read), with a secret made for
// the run: the proxy challenges any request without it (407), as ffmpeg
// expects, and serves nothing to anyone who can't answer.

// proxyUser is the user name in the proxy's credentials; the secret is
// what matters.
const proxyUser = "fog"

// dataProxy is the handler for one run, whose data route on fog is server
// (/compute/v1/data/<token>), serving whoever presents secret.
func (r *Runner) dataProxy(server, secret string) http.Handler {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyUser+":"+secret))
	fog, err := url.Parse(strings.TrimRight(r.Fog, "/"))
	if err != nil || (fog.Scheme != "http" && fog.Scheme != "https") || fog.Host == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "fog-node: fog's address isn't one", http.StatusBadGateway)
		})
	}
	transport := http.DefaultTransport
	if r.HTTP != nil && r.HTTP.Transport != nil {
		transport = r.HTTP.Transport
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(fog)
			pr.Out.URL.Path = strings.TrimRight(fog.Path, "/") + server + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = fog.Host
			// What the run says about itself stays with it.
			for _, h := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Forwarded", "X-Forwarded-For",
				"X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Del(compute.HeaderBodySHA256)
			r.signData(pr.Out, "")
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "fog-node: fog didn't answer: "+err.Error(), http.StatusBadGateway)
		},
	}
	prefix := compute.AppDataPrefix
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Proxy-Authorization")), []byte(want)) != 1 {
			w.Header().Set("Proxy-Authenticate", `Basic realm="fog-node run"`)
			http.Error(w, "fog-node: this run's ffmpeg only", http.StatusProxyAuthRequired)
			return
		}
		req.Header.Del("Proxy-Authorization")
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "fog-node: a run only reads through here", http.StatusMethodNotAllowed)
			return
		}
		p := req.URL.Path
		if !strings.HasPrefix(p, prefix) || path.Clean(p) != p || strings.Contains(p, "\\") ||
			(req.URL.RawPath != "" && !strings.HasPrefix(req.URL.RawPath, prefix)) {
			http.Error(w, "fog-node: not this run's data", http.StatusNotFound)
			return
		}
		proxy.ServeHTTP(w, req)
	})
}

// signData MACs a request on a run's data route as this machine: a read,
// or an upload whose body digest is bodySHA.
func (r *Runner) signData(req *http.Request, bodySHA string) {
	if r.Auth == nil {
		return
	}
	node, digest := r.Auth()
	nonce, at := compute.NewNonce(), time.Now().UTC().Format(time.RFC3339Nano)
	req.Header.Set(compute.HeaderNodeID, node)
	req.Header.Set(compute.HeaderNodeNonce, nonce)
	req.Header.Set(compute.HeaderNodeTime, at)
	if bodySHA != "" {
		req.Header.Set(compute.HeaderBodySHA256, bodySHA)
	}
	req.Header.Set(compute.HeaderNodeAuth, compute.DataMAC(digest, node, nonce, at, req.Method, req.URL.RequestURI(), bodySHA))
}

// dataProxyTimeout bounds how long the proxy waits for a request's
// headers: ffmpeg sends them at once.
const dataProxyTimeout = 10 * time.Second
