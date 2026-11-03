// Package firstboot consumes the configuration a bootable installer stages
// under the data dir: the admin account, server identity, and one-shot login
// token the setup wizard collected before the installed system ever booted.
// With it the operator types nothing twice — fogd comes up already set up,
// and the browser that drove the install claims a session and lands on the
// dashboard (see the /api/setup/claim handler).
package firstboot

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/store"
)

// FileName under the data dir. fog-setup writes it into the target's
// /var/lib/fog during installation.
const FileName = "firstboot.json"

// DefaultClaimTTL is the claim window installers stage.
const DefaultClaimTTL = "24h"

// Config is what the installer stages. The password arrives pre-hashed —
// the plaintext never touches the target disk — and only the claim token's
// digest is here, so the file itself can't sign anyone in.
type Config struct {
	Username         string `json:"username"`
	PasswordHash     string `json:"passwordHash"`
	ServerName       string `json:"serverName"`
	ClaimTokenDigest string `json:"claimTokenDigest"`
	// The claim deadline, as a Go duration ("24h"). It counts from the
	// moment this file is consumed, stamped by the installed system's own
	// chrony-synced clock — the live installer's clock may never have seen
	// NTP, and an absolute stamp from it could shrink or void the window.
	// The installer and this fogd ship in one image, so no older absolute
	// form can reach here.
	ClaimTokenTTL string `json:"claimTokenTTL,omitempty"`
}

// Consume applies a staged firstboot.json and removes it. A file that can't
// be applied is set aside as .rejected instead of retried forever — an
// installer bug must not be able to wedge every subsequent boot.
func Consume(dataDir string, st *store.Store, log *slog.Logger) error {
	path := filepath.Join(dataDir, FileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := apply(raw, st, log); err != nil {
		// The set-aside must actually happen before the message claims it
		// did: if the rename fails silently, every boot retries the same bad
		// file — the exact loop this mechanism exists to prevent. Removal is
		// the fallback (the raw content is already in this error's context
		// via the reject reason), and when even that fails the error names
		// the path so a human can break the loop.
		if rerr := os.Rename(path, path+".rejected"); rerr != nil {
			if rmerr := os.Remove(path); rmerr != nil {
				return fmt.Errorf("firstboot config rejected (%v) — and it could not be set aside or removed, so every boot will retry it; delete %s by hand: %v", err, path, rmerr)
			}
			return fmt.Errorf("firstboot config rejected (removed; setting it aside as .rejected failed: %v): %w", rerr, err)
		}
		return fmt.Errorf("firstboot config rejected (kept as %s.rejected): %w", FileName, err)
	}
	return os.Remove(path)
}

func apply(raw []byte, st *store.Store, log *slog.Logger) error {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing: %w", err)
	}
	n, err := st.CountOperators()
	if err != nil {
		return err
	}
	if n > 0 {
		return errors.New("setup already completed — refusing to preseed over an existing account")
	}
	cfg.Username = strings.TrimSpace(cfg.Username)
	if cfg.Username == "" {
		return errors.New("username is required")
	}
	// The preseeded name is the handle the operator will sign in with, so it
	// answers to the same rules every other handle does.
	handle, err := people.NormalizeHandle(cfg.Username)
	if err != nil {
		return fmt.Errorf("username %w", err)
	}
	if !strings.HasPrefix(cfg.PasswordHash, "$argon2id$") {
		return errors.New("passwordHash must be an argon2id hash")
	}
	if _, err := st.CreateFirstOperator(handle, cfg.Username, cfg.PasswordHash); err != nil {
		if errors.Is(err, store.ErrSetupDone) {
			return errors.New("setup already completed — refusing to preseed over an existing account")
		}
		return err
	}
	if v := strings.TrimSpace(cfg.ServerName); v != "" {
		if err := st.SetSetting(store.SettingServerName, v); err != nil {
			return fmt.Errorf("recording the server name: %w", err)
		}
	}
	// This flag failing is not ignorable: without it the setup wizard still
	// opens — over a database that already holds the admin account created
	// just above.
	if err := st.SetSetting(store.SettingSetupDone, "1"); err != nil {
		return fmt.Errorf("marking setup done: %w", err)
	}
	// The one-shot login token: optional, and dropped (not fatal) when its
	// deadline doesn't parse or its settings won't write — an unclaimable
	// token still leaves a fully working login page. The digest is written
	// last so a partial write can never leave a claimable token with no
	// deadline or user behind it.
	if cfg.ClaimTokenDigest != "" {
		expires, derr := claimDeadline(cfg)
		if derr != nil {
			log.Warn("firstboot: claim token deadline unusable — token dropped", "err", derr)
		} else {
			err := st.SetSetting(store.SettingClaimExpires, expires)
			if err == nil {
				err = st.SetSetting(store.SettingClaimUser, cfg.Username)
			}
			if err == nil {
				// The digest is the claim gate: written only once its
				// deadline and user are already on disk.
				err = st.SetSetting(store.SettingClaimDigest, cfg.ClaimTokenDigest)
			}
			if err != nil {
				log.Warn("firstboot: claim token could not be recorded — token dropped", "err", err)
			}
		}
	}
	st.Audit(cfg.Username, "setup.preseed", "", "account and identity staged by the installer")
	log.Info("firstboot: preseeded setup from installer", "user", cfg.Username)
	return nil
}

// claimDeadline resolves the claim window's absolute deadline, stamped from
// this machine's clock, now, at consume time — the installer's clock may have
// been wrong when it staged the file.
func claimDeadline(cfg Config) (string, error) {
	ttl := strings.TrimSpace(cfg.ClaimTokenTTL)
	d, err := time.ParseDuration(ttl)
	if err != nil || d <= 0 {
		return "", fmt.Errorf("ttl %q is not a positive duration", ttl)
	}
	return time.Now().Add(d).UTC().Format(time.RFC3339), nil
}
