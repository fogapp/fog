// Package compute lends fog the hardware of other machines in the house
// (docs/compute-contract.md). A machine runs fog-node and is paired
// once; from then on its hardware joins a pool, and every app that declares
// fog-compute/1 (docs/compute-contract.md) sees each pool as one transcoder
// that fog provides. fogd relays: it polls each app as that transcoder,
// hands each run the app starts to a machine in the pool with room, and
// reports back what ran. The bytes go between the machine and the app
// through the data route, never through the relay.
//
// The wire types for both sides live here: what a node and fogd say to
// each other (/compute/v1), and what fogd and an app say (/fog/compute).
package compute

// Contract is the app-side contract an app's fog.yaml declares.
const Contract = "fog-compute/1"

// Classes of work. Interactive is a person watching; batch can wait.
const (
	ClassInteractive = "interactive"
	ClassBatch       = "batch"
)

// Slots is room per class.
type Slots struct {
	Interactive int `json:"interactive"`
	Batch       int `json:"batch"`
}

// VideoCaps is what a machine can do with ffmpeg, as its node probed it.
type VideoCaps struct {
	// Family is the hardware family a real frame encoded through:
	// "videotoolbox", "vaapi", or "software". It decides the pool.
	Family   string   `json:"family"`
	FFmpeg   string   `json:"ffmpeg"`
	Encoders []string `json:"encoders,omitempty"`
	Filters  []string `json:"filters,omitempty"`
	// Slots is the machine's budget: how many runs of each class at once.
	Slots Slots `json:"slots"`
	// Confinement is how the node confines each run's ffmpeg, proven by a
	// run that tried to read past it: "sandbox-exec" on a Mac, "landlock" on
	// Linux (Landlock, namespaces and seccomp). "" when it can't, with
	// ConfinementError saying why; such a machine joins no video pool.
	Confinement      string `json:"confinement,omitempty"`
	ConfinementError string `json:"confinementError,omitempty"`
	// Unconfined is the machine's owner running ffmpeg without confinement
	// (fog-node unconfined on). fog lends it anyway, and says so.
	Unconfined bool `json:"unconfined,omitempty"`
	// Limits are the most the node lets one run use; a run may ask for less.
	Limits *RunLimits `json:"limits,omitempty"`
}

// RunLimits bound one run on its machine. Zero is no limit of fog's: the
// node's own still holds.
type RunLimits struct {
	// Seconds is the longest a run may last, start to exit.
	Seconds int64 `json:"seconds,omitempty"`
	// ScratchBytes is the most its scratch directory may hold at once.
	ScratchBytes int64 `json:"scratchBytes,omitempty"`
}

// Lower answers the tighter of two limits, field by field; zero is none.
func (l RunLimits) Lower(o RunLimits) RunLimits {
	lower := func(a, b int64) int64 {
		if a <= 0 || (b > 0 && b < a) {
			return max(b, 0)
		}
		return a
	}
	return RunLimits{Seconds: lower(l.Seconds, o.Seconds), ScratchBytes: lower(l.ScratchBytes, o.ScratchBytes)}
}

// InferenceCaps is a model server the node fronts.
type InferenceCaps struct {
	// API is the protocol the server speaks: "openai".
	API string `json:"api"`
	// URL is where fogd reaches the node's inference listener.
	URL    string   `json:"url"`
	Models []string `json:"models,omitempty"`
	// Local is the node saying its model server is on the machine itself:
	// fog-node reaches it on a loopback address. It's a claim about the
	// first hop and no further; see docs/compute-contract.md. A node from
	// before this field reports nothing, which reads as false.
	Local bool `json:"local,omitempty"`
}

// Inventory is what a node reports about its machine.
type Inventory struct {
	Name        string         `json:"name"`
	OS          string         `json:"os"`
	Arch        string         `json:"arch"`
	Version     string         `json:"version"`
	CPUs        int            `json:"cpus,omitempty"`
	MemoryBytes uint64         `json:"memoryBytes,omitempty"`
	Video       *VideoCaps     `json:"video,omitempty"`
	Inference   *InferenceCaps `json:"inference,omitempty"`
	// Busy is the machine's owner saying it's needed for something else:
	// it takes no batch work while set.
	Busy bool `json:"busy,omitempty"`
	// UpdateFailed is an update fog offered that the node couldn't put in
	// place, and why; nil once it runs fog's release.
	UpdateFailed *UpdateFailed `json:"updateFailed,omitempty"`
}

// UpdateFailed is why a node still runs the release it does.
type UpdateFailed struct {
	Version string `json:"version"`
	Error   string `json:"error"`
}

// Exited is a run that ended: Status is ffmpeg's exit status, nil when it
// was killed or lost; Log is the end of what it said.
type Exited struct {
	ID     string `json:"id"`
	Status *int   `json:"status"`
	Log    string `json:"log,omitempty"`
}

// --- node ⇄ fogd (/compute/v1) ---------------------------------------------

// JoinRequest pairs a machine with a one-time code, without sending the
// code. Each live code has a public salt (GET /compute/v1/join/salts); the
// machine doesn't know which is its code's, so it makes an attempt under
// each: Proof is JoinProof over Nonce under JoinKey(code, salt), and
// Credential the digest of a credential the machine made itself, sealed
// under that key (SealCredential; trust.go).
type JoinRequest struct {
	Nonce     string        `json:"nonce"`
	Attempts  []JoinAttempt `json:"attempts"`
	Inventory Inventory     `json:"inventory"`
}

// JoinAttempt is a pairing under one code's salt.
type JoinAttempt struct {
	Salt       string `json:"salt"`
	Proof      string `json:"proof"`
	Credential string `json:"credential"`
}

// JoinSalts are the salts of the live codes.
type JoinSalts struct {
	Salts []string `json:"salts"`
}

// MaxJoinAttempts bounds a pairing's attempts: one per live code.
const MaxJoinAttempts = 16

// RotateRequest and RotateAnswer are a machine's credential rotation, right
// after pairing, carried by a poll (NodeReport.Rotate, NodeAnswer.Rotated):
// each side's ephemeral X25519 key, base64; the new digest is
// RotatedDigest of their shared secret.
type RotateRequest struct {
	Pub string `json:"pub"`
}

type RotateAnswer struct {
	Pub string `json:"pub"`
}

// JoinAnswer is the node's id and fog's instance key for the node to pin;
// Confirm is fog's proof that it holds the code too, over all of it and
// the credential it took (JoinConfirm).
type JoinAnswer struct {
	Node    string `json:"node"`
	FogKey  string `json:"fogKey"`
	Confirm string `json:"confirm"`
}

// NodeReport is one poll from a node.
type NodeReport struct {
	// Inventory, when the machine changed (and on the first poll).
	Inventory *Inventory         `json:"inventory,omitempty"`
	Running   []string           `json:"running"`
	Exited    []Exited           `json:"exited,omitempty"`
	Progress  map[string]float64 `json:"progress,omitempty"`
	// Wait lets fogd hold the answer until it has work for the node.
	Wait bool `json:"wait,omitempty"`
	// Rotate asks for a new credential (the first poll after pairing).
	Rotate *RotateRequest `json:"rotate,omitempty"`
}

// Outputs says which files a run uploads, and when. Names are relative to
// the run's scratch directory ({out}).
type Outputs struct {
	// Init files go first, once a Stream file exists or the run ended.
	Init []string `json:"init,omitempty"`
	// Stream is a glob of files uploaded as soon as each appears (ffmpeg
	// writes them under their final name only once complete), each
	// deleted once uploaded.
	Stream string `json:"stream,omitempty"`
	// Final files are uploaded after a clean exit.
	Final []string `json:"final,omitempty"`
}

// NodeRun is one run fogd gives a node.
type NodeRun struct {
	ID    string   `json:"id"`
	Class string   `json:"class"`
	Argv  []string `json:"argv"`
	// Server replaces {server} in argv and Upload: the app's base on the
	// data route.
	Server  string  `json:"server"`
	Outputs Outputs `json:"outputs"`
	// Upload is where each output goes, with {server} and {name} to fill.
	Upload string `json:"upload"`
	// Limits is what the run may use, when less than the node's own.
	Limits *RunLimits `json:"limits,omitempty"`
}

// Update is a newer fog-node for the node to replace itself with.
type Update struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	// Retry has the node try again a version it failed to update to: the
	// operator pressed Update now.
	Retry bool `json:"retry,omitempty"`
	// Manifest and ManifestSig are the release's latest.json and its
	// signature, exactly as fog's update source signed them: the node
	// installs the build only if the manifest verifies against the release
	// key it pins and names the build's digest. SHA256 is fog's word, for
	// the record; the manifest's is the one checked.
	Manifest    []byte `json:"manifest,omitempty"`
	ManifestSig string `json:"manifestSig,omitempty"`
}

// NodeAnswer answers a poll.
type NodeAnswer struct {
	Start  []NodeRun `json:"start,omitempty"`
	Stop   []string  `json:"stop,omitempty"`
	PollMS int       `json:"pollMs"`
	Update *Update   `json:"update,omitempty"`
	// Removed: the operator removed this machine while its poll was held.
	// The node stops its runs, forgets its pairing and stops lending. (A
	// poll that starts after the removal is a 401, which means the same.)
	Removed bool `json:"removed,omitempty"`
	// Rotated is fog's half of a rotation the poll asked for: from this
	// answer on, fog takes the new credential (and the old, until it hears
	// the new).
	Rotated *RotateAnswer `json:"rotated,omitempty"`
}

// --- fogd ⇄ app (fog-compute/1) --------------------------------------------

// Paths on the app.
const (
	AppTranscodersPath = "/fog/compute/transcoders"
	// AppDataPrefix is the only part of an app the data route reaches:
	// the inputs and uploads the app signs for each run.
	AppDataPrefix = "/fog/compute/"
)

// Hello introduces a pool to an app, in the shape a remote transcoder's
// worker uses for itself.
type Hello struct {
	Encoder  string   `json:"encoder"`
	Hostname string   `json:"hostname"`
	Platform string   `json:"platform"`
	FFmpeg   string   `json:"ffmpeg"`
	Worker   string   `json:"worker"`
	Filters  []string `json:"filters"`
}

// TranscoderRequest asks an app to know a pool as one of its transcoders.
type TranscoderRequest struct {
	Pool  string `json:"pool"`
	Name  string `json:"name"`
	Hello Hello  `json:"hello"`
}

// TranscoderAnswer is the app's name for it.
type TranscoderAnswer struct {
	ID string `json:"id"`
}

// AppReport is one poll of an app, as its pool's transcoder.
type AppReport struct {
	Hello    *Hello             `json:"hello,omitempty"`
	Running  []string           `json:"running"`
	Exited   []Exited           `json:"exited"`
	Progress map[string]float64 `json:"progress"`
	// Free is the room the pool has for this app now; Capacity all it
	// would have idle.
	Free     Slots `json:"free"`
	Capacity Slots `json:"capacity"`
}

// AppRun is a run an app starts.
type AppRun struct {
	ID string `json:"id"`
	// Kind is the worker's own word ("session", "optimize"), read when the
	// app sent no Class or Outputs.
	Kind    string   `json:"kind,omitempty"`
	Class   string   `json:"class,omitempty"`
	Argv    []string `json:"argv"`
	Outputs *Outputs `json:"outputs,omitempty"`
	Upload  string   `json:"upload,omitempty"`
	// Limits lowers the machine's own limits for this run.
	Limits *RunLimits `json:"limits,omitempty"`
}

// AppAnswer answers an app poll.
type AppAnswer struct {
	Start  []AppRun `json:"start"`
	Stop   []string `json:"stop"`
	PollMS int      `json:"poll_ms"`
}
