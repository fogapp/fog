package computenode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
)

// Inference: the machine's own model server (think, Ollama, MLX's, LM
// Studio), lent to fog. fog-node fronts it on the address it reaches fog
// from, takes only fog's key, and forwards only /v1/, so the model server
// itself never has to listen beyond the machine.

// The model servers fog-node finds by itself, where they listen by default.
// think comes first: its lending door offers only what the owner chose to
// lend, where Ollama's API offers everything it has.
const (
	ThinkUpstream  = "http://127.0.0.1:8485"
	OllamaUpstream = "http://127.0.0.1:11434"
)

// knownUpstreams is what's looked for when nothing is configured.
var knownUpstreams = []string{ThinkUpstream, OllamaUpstream}

// DefaultInferenceListen is where fog-node fronts it for fog.
const DefaultInferenceListen = ":7718"

// InferenceSecret is the key fog MACs its requests to this machine's
// inference listener with, never sent: derived from the machine's own
// credential, so no second secret is kept on either side
// (compute.InferenceKey).
func InferenceSecret(token string) string {
	return compute.InferenceKey(compute.TokenDigest(token))
}

// upstream answers the model server to front: the configured one, else the
// first known one that answers. "" for none.
func (c *Config) upstream(ctx context.Context) string {
	if c.InferenceOff {
		return ""
	}
	candidates := knownUpstreams
	if c.InferenceUpstream != "" {
		candidates = []string{c.InferenceUpstream}
	}
	for _, u := range candidates {
		if _, err := listModels(ctx, u); err == nil {
			return strings.TrimRight(u, "/")
		}
	}
	return ""
}

// onThisMachine reports whether upstream is a loopback address, by the name
// fog-node dials: a URL that names this machine's LAN address, or anywhere
// else, is not. Which server answers there, and where that server sends a
// prompt, is the owner's to know.
func onThisMachine(upstream string) bool {
	u, err := url.Parse(upstream)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listModels asks an OpenAI-compatible server which models it has.
func listModels(ctx context.Context, upstream string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(upstream, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != 200 {
		return nil, &url.Error{Op: "GET", URL: req.URL.String(), Err: http.ErrNotSupported}
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	models := []string{}
	for _, m := range list.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// lanAddress is this machine's address on the way to fog: what fog dials
// to reach the inference listener.
func lanAddress(fog string) string {
	u, err := url.Parse(fog)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	conn, err := net.Dial("udp", host)
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// Lender fronts the machine's model server for fog. It is the one place
// that says which server that is: the inventory fog routes by and the
// listener that answers fog's requests both read what it found last, so
// fog is never told of one server's models while the listener forwards to
// another. It looks again every few seconds, so a server that starts later
// (think after Ollama, say) is lent within moments, and says when what it
// lends changed, so the agent tells fog at once.
type Lender struct {
	Config *Config
	Log    *slog.Logger
	// Every is how often the model server is looked for again (default
	// five seconds).
	Every time.Duration

	mu       sync.Mutex
	upstream string
	caps     *compute.InferenceCaps
	changed  chan struct{}
	srv      *http.Server
	bound    string
	bindErr  string
}

// inferenceRetry is how often the model server is looked for.
const inferenceRetry = 5 * time.Second

func (l *Lender) init() {
	if l.changed == nil {
		l.changed = make(chan struct{}, 1)
	}
}

// Changed is signalled when what the machine lends changes.
func (l *Lender) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	return l.changed
}

// Inventory is what the machine lends now, for fog; nil for nothing.
func (l *Lender) Inventory() *compute.InferenceCaps {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.caps == nil {
		return nil
	}
	c := *l.caps
	c.Models = slices.Clone(c.Models)
	return &c
}

// Run lends the model server until ctx ends, looking again every Every.
func (l *Lender) Run(ctx context.Context) {
	every := l.Every
	if every == 0 {
		every = inferenceRetry
	}
	defer l.stop()
	for {
		l.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Refresh looks for the model server and its models now, and lends what
// it finds: the listener bound, or closed when there's nothing to lend.
func (l *Lender) Refresh(ctx context.Context) {
	cfg := l.Config
	up := cfg.upstream(ctx)
	var models []string
	if up != "" {
		var err error
		if models, err = listModels(ctx, up); err != nil {
			up = ""
		}
	}
	addr, url := inferenceAddr(cfg)
	if up == "" || addr == "" {
		l.set("", nil)
		l.unbind()
		return
	}
	if err := l.bind(addr); err != nil {
		l.set("", nil)
		return
	}
	l.set(up, &compute.InferenceCaps{API: "openai", URL: url, Models: models, Local: onThisMachine(up)})
}

// set changes what's lent, both halves at once, and says so when it
// changed.
func (l *Lender) set(upstream string, caps *compute.InferenceCaps) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	was, _ := json.Marshal(l.caps)
	now, _ := json.Marshal(caps)
	l.upstream, l.caps = upstream, caps
	if string(was) != string(now) {
		if l.Log != nil {
			if caps == nil {
				l.Log.Info("not lending a model server")
			} else {
				l.Log.Info("lending a model server to fog", "server", upstream, "models", len(caps.Models), "listen", l.bound)
			}
		}
		select {
		case l.changed <- struct{}{}:
		default:
		}
	}
}

// inferenceAddr is where the listener binds, and the URL fog is told: this
// machine's address on the way to fog, and nowhere else it has, unless the
// owner named an address of their own.
func inferenceAddr(cfg *Config) (bind, url string) {
	listen := cfg.InferenceListen
	if listen == "" {
		listen = DefaultInferenceListen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", ""
	}
	lan := lanAddress(cfg.Fog)
	if lan == "" {
		return "", ""
	}
	if host == "" {
		host = lan
	}
	url = "http://" + net.JoinHostPort(lan, port)
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		url = "http://" + net.JoinHostPort(host, port)
	}
	return net.JoinHostPort(host, port), url
}

// bind listens at addr, moving the listener when the address changed.
func (l *Lender) bind(addr string) error {
	l.mu.Lock()
	if l.srv != nil && l.bound == addr {
		l.mu.Unlock()
		return nil
	}
	old := l.srv
	l.srv, l.bound = nil, ""
	l.mu.Unlock()
	if old != nil {
		old.Close()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		l.mu.Lock()
		first := l.bindErr != err.Error()
		l.bindErr = err.Error()
		l.mu.Unlock()
		if first && l.Log != nil {
			l.Log.Warn("can't lend the model server: its listener can't listen", "addr", addr, "err", err)
		}
		return err
	}
	srv := &http.Server{Handler: l.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	l.mu.Lock()
	l.srv, l.bound, l.bindErr = srv, addr, ""
	l.mu.Unlock()
	return nil
}

func (l *Lender) unbind() {
	l.mu.Lock()
	old := l.srv
	l.srv, l.bound = nil, ""
	l.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// stop lends nothing more: the listener closed, and nothing reported.
func (l *Lender) stop() {
	l.set("", nil)
	l.unbind()
}

// Handler fronts whichever model server is lent now, for fog: only its /v1
// API, only with fog's key, streamed straight through.
func (l *Lender) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read each time: a rotation changes it.
		secret := compute.InferenceKey(l.Config.Secret())
		l.mu.Lock()
		up := l.upstream
		l.mu.Unlock()
		if up == "" {
			if _, ok := fromFog(r, secret); !ok {
				http.Error(w, "fog only", http.StatusUnauthorized)
				return
			}
			http.Error(w, "this machine isn't lending a model server right now", http.StatusServiceUnavailable)
			return
		}
		InferenceHandler(up, secret).ServeHTTP(w, r)
	})
}

// maxInferenceBody bounds a request fog relays, as fog bounds it.
const maxInferenceBody = 32 << 20

// inferenceNonces are the nonces of fog's requests taken lately.
var inferenceNonces compute.Nonces

// listenerStart is when this fog-node started: nothing fog stamped before
// it is taken, since the nonces seen before it are gone.
var listenerStart = time.Now()

// fromFog answers a request's body when fog MACed the request with the
// inference key: its method, target, time, nonce, and body type and
// digest. The headers are checked first, inside the skew window, since
// this process started, and with a nonce not taken before; only then is
// the body read, and it must be the one fog declared. Nothing else is
// taken: there is no key on the wire to steal or replay.
func fromFog(r *http.Request, secret string) ([]byte, bool) {
	nonce, at, sum := r.Header.Get(compute.HeaderInferenceNonce), r.Header.Get(compute.HeaderInferenceTime), r.Header.Get(compute.HeaderBodySHA256)
	if len(nonce) < 16 || len(nonce) > 128 || len(sum) != 64 {
		return nil, false
	}
	if !compute.MACEqual(r.Header.Get(compute.HeaderInferenceAuth),
		compute.InferenceMAC(secret, nonce, at, r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), sum)) {
		return nil, false
	}
	now := time.Now()
	if _, ok := compute.Fresh(at, now, listenerStart); !ok || !inferenceNonces.Take(nonce, now, 2*compute.RequestSkew) {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInferenceBody+1))
	if err != nil || len(body) > maxInferenceBody || compute.BodySHA256(body) != sum {
		return nil, false
	}
	return body, true
}

// InferenceHandler fronts one model server for fog: only its /v1 API, only
// requests fog MACed with the inference key, streamed straight through.
func InferenceHandler(upstream, secret string) http.Handler {
	target, _ := url.Parse(upstream)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Authorization")
		},
		FlushInterval: -1,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := fromFog(r, secret)
		if !ok {
			http.Error(w, "fog only", http.StatusUnauthorized)
			return
		}
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		for _, h := range []string{compute.HeaderInferenceAuth, compute.HeaderInferenceNonce, compute.HeaderInferenceTime, compute.HeaderBodySHA256} {
			r.Header.Del(h)
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/") || strings.Contains(r.URL.Path, "..") || strings.Contains(r.URL.RawPath, "..") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
