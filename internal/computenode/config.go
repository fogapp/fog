package computenode

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"gitea.lan/matthew/fog/internal/compute"
	"os"
	"path/filepath"
	"runtime"
)

// Config is what a paired node keeps: where fog is, who it is to fog, and
// its credential. It lives in Dir, readable by its own user only.
type Config struct {
	// Fog is fogd's address, e.g. http://192.168.1.200:8080.
	Fog  string `json:"fog"`
	Node string `json:"node"`
	// Token is the credential the machine made at pairing; Credential,
	// once set, is the digest it rotated to right after (the only
	// credential from then on, and Token is gone). Neither ever crosses
	// the wire.
	Token      string `json:"token,omitempty"`
	Credential string `json:"credential,omitempty"`
	Rotated    bool   `json:"rotated,omitempty"`
	Name       string `json:"name"`
	// FogKey is fog's instance key, pinned at pairing: every answer fog
	// gives this machine is signed with it.
	FogKey string `json:"fogKey"`
	// FFmpeg pins the ffmpeg to use; empty finds one.
	FFmpeg string `json:"ffmpeg,omitempty"`
	// Busy is the owner taking the machine back from batch work.
	Busy bool `json:"busy,omitempty"`
	// The model server lent to fog: InferenceUpstream is the server
	// (default: think, then Ollama, on this machine), InferenceListen
	// where fog-node fronts it for fog (default :7718), InferenceOff not
	// lending one at all.
	InferenceUpstream string `json:"inferenceUpstream,omitempty"`
	InferenceListen   string `json:"inferenceListen,omitempty"`
	InferenceOff      bool   `json:"inferenceOff,omitempty"`
	// Unconfined is the owner choosing to run fog's ffmpeg without
	// confinement (fog-node unconfined on): on a machine that can't confine
	// it, the only way to lend video. fog shows it.
	Unconfined bool `json:"unconfined,omitempty"`
}

// ErrNotPaired is a node with no config yet.
var ErrNotPaired = errors.New("this machine isn't paired with fog yet: run fog-node join")

// ErrRepair is a pairing from before fog-node pinned fog's key: it can't
// tell fog from anyone else, so it lends nothing until paired again.
var ErrRepair = fmt.Errorf("%w (it was paired before fog-node checked fog's identity: remove it in fog's Settings › Compute and pair it again)", ErrNotPaired)

// DefaultDir is where a node keeps its state: per user on a Mac (the agent
// runs in the owner's session), system-wide for root on Linux.
func DefaultDir() string {
	home, _ := os.UserHomeDir()
	switch {
	case runtime.GOOS == "darwin":
		return filepath.Join(home, "Library", "Application Support", "fog-node")
	case os.Geteuid() == 0:
		return "/var/lib/fog-node"
	default:
		return filepath.Join(home, ".local", "share", "fog-node")
	}
}

func configPath(dir string) string { return filepath.Join(dir, "config.json") }

// LoadConfig reads a node's config.
func LoadConfig(dir string) (*Config, error) {
	raw, err := os.ReadFile(configPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotPaired
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if (c.Token == "" && c.Credential == "") || c.Fog == "" {
		return nil, ErrNotPaired
	}
	if c.FogKey == "" {
		return &c, ErrRepair
	}
	return &c, nil
}

// secret guards Credential while a rotation changes it under the
// listener that derives its key from it.
var secret sync.Mutex

// Secret is the digest the machine MACs with: what fog keeps of its
// credential.
func (c *Config) Secret() string {
	secret.Lock()
	defer secret.Unlock()
	if c.Credential != "" {
		return c.Credential
	}
	return compute.TokenDigest(c.Token)
}

// rotated takes the credential a rotation agreed on, for good.
func (c *Config) rotated(digest string) {
	secret.Lock()
	defer secret.Unlock()
	c.Credential, c.Token, c.Rotated = digest, "", true
}

// Save writes the config, replacing it whole.
func (c *Config) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	secret.Lock()
	raw, err := json.MarshalIndent(c, "", "  ")
	secret.Unlock()
	if err != nil {
		return err
	}
	tmp := configPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath(dir))
}

// Forget removes the pairing: fog removed this machine.
func Forget(dir string) error {
	err := os.Remove(configPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
