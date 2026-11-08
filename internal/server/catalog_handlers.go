package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"gitea.lan/matthew/fog/internal/build"
	"gitea.lan/matthew/fog/internal/catalog"
	"gitea.lan/matthew/fog/internal/vault"
)

// The store's catalogs (catalog/registry.go): the list is the
// catalog_sources setting, the clones live under <data>/catalogs, and a
// loop fetches them every few hours or when the list changes.

// secretCatalogKey is the system secret holding the ssh key private
// catalogs are cloned with. Its public half is shown on Settings › App
// store, for the operator to add as a read-only deploy key.
const secretCatalogKey = "catalog_deploy_key"

const catalogSyncInterval = 6 * time.Hour

func newCatalogRegistry(s *Server) *catalog.Registry {
	reg := &catalog.Registry{
		Dir: filepath.Join(s.cfg.DataDir, "catalogs"),
		Log: s.log,
		Key: func() (string, error) { return s.catalogKey() },
	}
	raw, _ := s.store.GetSetting(catalog.SettingRemotes)
	remotes, err := catalog.ParseRemotes(raw)
	if err != nil {
		// Only a hand-edited database gets here: the settings API refuses
		// a list it can't parse.
		s.log.Warn("catalog list unreadable; using fog's own", "err", err)
		remotes = catalog.DefaultRemotes
	}
	reg.SetRemotes(remotes)
	return reg
}

// catalogKey is the private half of this fog's catalog key, minted the
// first time it is asked for.
func (s *Server) catalogKey() (string, error) {
	if s.vault == nil {
		return "", errors.New("no vault")
	}
	return vault.Secrets{Store: s.store, Vault: s.vault}.GetOrMint(0, secretCatalogKey, func() (string, error) {
		priv, _, err := build.GenerateDeployKey()
		return priv, err
	})
}

// catalogPublicKey is the line to paste into a git host as a deploy key.
func (s *Server) catalogPublicKey() string {
	priv, err := s.catalogKey()
	if err != nil {
		return ""
	}
	signer, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " fog-catalogs"
}

// CatalogLoop keeps the catalogs fetched; fogd runs it for the life of the
// process.
func (s *Server) CatalogLoop(ctx context.Context) {
	s.catalogs.Loop(ctx, catalogSyncInterval, s.catalogWake)
}

// wakeCatalogs asks the loop to fetch now; a wake already pending covers
// this one.
func (s *Server) wakeCatalogs() {
	select {
	case s.catalogWake <- struct{}{}:
	default:
	}
}

// catalogsChanged applies a saved catalog list: reread what is on disk
// for it at once, and fetch what is new.
func (s *Server) catalogsChanged() {
	raw, _ := s.store.GetSetting(catalog.SettingRemotes)
	remotes, err := catalog.ParseRemotes(raw)
	if err != nil {
		return
	}
	s.catalogs.SetRemotes(remotes)
	s.wakeCatalogs()
}

// handleCatalogSync fetches every catalog now and answers how each stands.
func (s *Server) handleCatalogSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	s.catalogs.Sync(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"catalogs": s.catalogs.Statuses()})
}

// handleCatalogAsset serves a listing's artwork from the catalog it came
// with. A catalog is somebody else's repository, so its SVGs are served
// sandboxed: as images they can't run script anyway, and opened directly
// they get no origin to run it in.
func (s *Server) handleCatalogAsset(w http.ResponseWriter, r *http.Request) {
	f, typ, err := s.catalogs.Asset(r.PathValue("catalog"), r.PathValue("id"), r.PathValue("file"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", st.ModTime(), rs)
		return
	}
	io.Copy(w, io.LimitReader(f, 8<<20))
}
