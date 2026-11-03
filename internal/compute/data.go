package compute

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"

	"gitea.lan/matthew/fog/internal/store"
)

// DataPath is fogd's data route: a machine reaches the app it works for
// at DataPath + <run's data token> + the app's own /fog/compute/… path.
const DataPath = "/compute/v1/data/"

// ErrNoRun is a data token for no live run.
var ErrNoRun = errors.New("no such run")

// dataApp answers the app a live run's data token is for, and the channel
// closed when that run ends, when the run is node's.
func (r *Relay) dataApp(token, node string) (*store.App, <-chan struct{}, error) {
	if token == "" {
		return nil, nil, ErrNoRun
	}
	r.mu.Lock()
	var found *run
	for _, rn := range r.runs {
		if subtle.ConstantTimeCompare([]byte(rn.Data), []byte(token)) == 1 {
			found = rn
		}
	}
	r.mu.Unlock()
	if found == nil || found.Node != node {
		return nil, nil, ErrNoRun
	}
	app, err := r.Store.AppByID(found.AppID)
	return app, found.done, err
}

// Resolve answers an app's base on the fog network ("http://10.0.3.4:3000").
type Resolve func(ctx context.Context, app *store.App) (string, error)

// DataHandler forwards a machine's request for a run's input or upload to
// the app, and nowhere else in it: only paths under /fog/compute/, only
// reads and whole-file uploads, only while the run lives, and only from
// the machine the run is on, proven by its MAC over the request
// (DataMAC): the run's token, in the path, only says which run, and alone
// opens nothing. A request still going when the run ends is cut off then;
// the app checks its own signature on what follows.
func (r *Relay) DataHandler(resolve Resolve) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodPut:
		default:
			http.Error(w, "a run reads its inputs and uploads its outputs, nothing else", http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(req.URL.Path, DataPath)
		token, appPath, ok := strings.Cut(rest, "/")
		appPath = "/" + appPath
		if !ok || !strings.HasPrefix(appPath, AppDataPrefix) || path.Clean(appPath) != appPath || strings.Contains(appPath, "\\") {
			http.Error(w, "not a run's data", http.StatusNotFound)
			return
		}
		node, sum := req.Header.Get(HeaderNodeID), req.Header.Get(HeaderBodySHA256)
		// A read has no body; an upload declares its digest, which the MAC
		// covers and its bytes must match on the way through.
		if (req.Method == http.MethodPut) != (len(sum) == 64) {
			http.Error(w, "an upload declares its body's digest, and a read has none", http.StatusBadRequest)
			return
		}
		if _, err := r.Service.Registry.AuthenticateData(node, req.Header.Get(HeaderNodeNonce), req.Header.Get(HeaderNodeTime),
			req.Header.Get(HeaderNodeAuth), req.Method, req.URL.RequestURI(), sum); err != nil {
			http.Error(w, "not from the machine running this", http.StatusUnauthorized)
			return
		}
		if req.Method == http.MethodPut {
			if req.ContentLength < 0 {
				http.Error(w, "an upload says its length", http.StatusLengthRequired)
				return
			}
			req.Body = struct {
				io.Reader
				io.Closer
			}{&DigestReader{R: req.Body, Want: sum, Size: req.ContentLength}, req.Body}
		}
		app, done, err := r.dataApp(token, node)
		if err != nil {
			http.Error(w, "no such run", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		go func() {
			select {
			case <-done:
				cancel()
			case <-ctx.Done():
			}
		}()
		req = req.WithContext(ctx)
		base, err := resolve(req.Context(), app)
		if err != nil {
			http.Error(w, app.Name+" can't be reached: "+err.Error(), http.StatusBadGateway)
			return
		}
		target, err := url.Parse(base)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
				pr.Out.URL.Path, pr.Out.URL.RawPath = appPath, ""
				pr.Out.URL.RawQuery = req.URL.RawQuery
				pr.Out.Host = target.Host
				// Nothing of the machine's or fog's reaches the app.
				for _, h := range []string{"Authorization", "Cookie", HeaderNodeID, HeaderNodeNonce, HeaderNodeTime, HeaderNodeAuth, HeaderBodySHA256} {
					pr.Out.Header.Del(h)
				}
			},
			// Fragments and media go straight through, never buffered.
			FlushInterval: -1,
		}
		proxy.ServeHTTP(w, req)
	})
}

// DataHandler is the relay's data route, or a 404 for everything when
// there's no relay.
func (s *Service) DataHandler(resolve Resolve) http.Handler {
	if s == nil || s.relay == nil {
		return http.NotFoundHandler()
	}
	return s.relay.DataHandler(resolve)
}
