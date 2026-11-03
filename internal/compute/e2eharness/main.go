//go:build e2e

// e2eharness is fog's whole compute path in one process, for an app's
// own end-to-end test of fog-compute/1 (Stream's spec/requests/fog_compute
// _e2e_spec.rb runs it): a store with the app declaring the contract, a
// machine paired with it, the relay polling the app as that machine's
// pool, the data route, and the real fog-node runner doing the work with
// this machine's ffmpeg.
//
//	APP_URL=http://127.0.0.1:3104 FOG_API_TOKEN=… [APP_NAME=once APP_CLASSES=batch] go run -tags e2e ./internal/compute/e2eharness
//
// It prints "ready <fog url>" once the machine is paired, then runs until
// killed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/compute"
	"gitea.lan/matthew/fog/internal/computenode"
	"gitea.lan/matthew/fog/internal/store"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == computenode.ConfineArg {
		fmt.Fprintln(os.Stderr, "e2eharness: ffmpeg couldn't be confined:", computenode.ConfineMain(os.Args[2:]))
		os.Exit(125)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2eharness:", err)
		os.Exit(1)
	}
}

func run() error {
	appURL, token := os.Getenv("APP_URL"), os.Getenv("FOG_API_TOKEN")
	if appURL == "" || token == "" {
		return fmt.Errorf("APP_URL and FOG_API_TOKEN are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dir, err := os.MkdirTemp("", "fog-e2e-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		return err
	}
	// APP_NAME and APP_CLASSES say which app this is and what work it
	// hands over (default: Stream's).
	name := os.Getenv("APP_NAME")
	if name == "" {
		name = "stream"
	}
	classes := os.Getenv("APP_CLASSES")
	if classes == "" {
		classes = "interactive,batch"
	}
	app, err := st.CreateApp(name)
	if err != nil {
		return err
	}
	declared, _ := json.Marshal(map[string]any{"contract": compute.Contract, "video": map[string]any{"classes": strings.Split(classes, ",")}})
	if err := st.SetAppCompute(app.ID, string(declared)); err != nil {
		return err
	}
	resolve := func(_ context.Context, list []store.App) []apps.AppEndpoint {
		out := make([]apps.AppEndpoint, len(list))
		for i := range list {
			out[i] = apps.AppEndpoint{Base: appURL, Token: token}
		}
		return out
	}
	svc := &compute.Service{Registry: &compute.Registry{Store: st, Log: log}, Log: log}
	relay := compute.NewRelay(svc, st, appcall.New(resolve).Refusing(compute.Refusal), log)

	mux := http.NewServeMux()
	// fog's instance identity, as the machine pins it at pairing.
	signer := compute.NewKeySigner()
	mux.HandleFunc("POST /compute/v1/join", func(w http.ResponseWriter, r *http.Request) {
		var req compute.JoinRequest
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Attempts) == 0 {
			salts, _ := svc.Registry.Salts()
			json.NewEncoder(w).Encode(compute.JoinSalts{Salts: salts})
			return
		}
		ans, err := svc.Registry.Join(req, signer.PublicKeyB64())
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(ans)
	})
	mux.HandleFunc("POST /compute/v1/poll", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		n, err := svc.Registry.AuthenticateRequest(r.Header.Get(compute.HeaderNodeID), r.Header.Get(compute.HeaderNodeNonce),
			r.Header.Get(compute.HeaderNodeTime), r.Header.Get(compute.HeaderNodeAuth), r.Header.Get("Content-Type"),
			r.Header.Get(compute.HeaderBodySHA256))
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var report compute.NodeReport
		if err := json.Unmarshal(body, &report); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		from, _, _ := net.SplitHostPort(r.RemoteAddr)
		ans, err := svc.Poll(r.Context(), n, report, from)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		raw, _ := json.Marshal(ans)
		w.Header().Set(compute.HeaderFogSignature, signer.SignTranscript(compute.AnswerLabel,
			compute.AnswerNonce(n.ID, r.Header.Get(compute.HeaderNodeNonce)), raw))
		w.Write(raw)
	})
	mux.Handle(compute.DataPath, svc.DataHandler(func(context.Context, *store.App) (string, error) { return appURL, nil }))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	fogURL := "http://" + ln.Addr().String()
	go http.Serve(ln, mux)
	go relay.Run(ctx)

	code, err := svc.Registry.NewCode("e2e")
	if err != nil {
		return err
	}
	nodeDir := filepath.Join(dir, "node")
	cfg, err := computenode.Join(ctx, nodeDir, fogURL, code, "e2e machine", "", "e2e")
	if err != nil {
		return err
	}
	// Confined as a machine's runs are: the helper is this binary.
	work := &computenode.Runner{Fog: fogURL, FFmpeg: computenode.FindFFmpeg(""), Work: filepath.Join(nodeDir, "work"), Log: log}
	work.Auth = func() (string, string) { return cfg.Node, cfg.Secret() }
	agent := &computenode.Agent{Dir: nodeDir, Config: cfg, Version: "e2e", Log: log, Work: work}
	fmt.Println("ready", fogURL)
	return agent.Run(ctx)
}
