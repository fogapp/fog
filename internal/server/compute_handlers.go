package server

// Compute nodes (docs/compute-contract.md): machines that lend fog
// their hardware. /compute/v1 is the machines' own surface, authenticated
// by each node's credential; /api/compute is Settings › Compute.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/compute"
	"gitea.lan/matthew/fog/internal/store"
)

// computeSigner is fog's instance identity, which machines pin at pairing
// and which signs every answer to them.
func (s *Server) computeSigner() (compute.Signer, error) {
	if s.compute != nil && s.compute.Signer != nil {
		return s.compute.Signer, nil
	}
	if s.fed == nil {
		return nil, errors.New("fog has no instance identity to sign with")
	}
	return s.fed.Identity()
}

// writeSignedCompute answers a machine's poll, signed for the machine and
// the nonce it sent, so it can tell fog's answer from anyone else's. A poll
// without a nonce (a fog-node from before) gets the same answer unsigned.
func (s *Server) writeSignedCompute(w http.ResponseWriter, r *http.Request, node string, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, err)
		return
	}
	if nonce := r.Header.Get(compute.HeaderNodeNonce); nonce != "" && len(nonce) <= 128 {
		signer, err := s.computeSigner()
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		w.Header().Set(compute.HeaderFogSignature, signer.SignTranscript(compute.AnswerLabel, compute.AnswerNonce(node, nonce), body))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// computeNode authenticates a machine's request by its MAC over the
// request (body); nil after answering why not. There is no other way in:
// a credential is never sent. Every refusal is signed for the machine it
// names (Fog-Node), and only CodeUnknownMachine, which says fog has no
// such machine, unpairs it. A request fog couldn't authenticate (tampered
// with, replayed, or from a clock that's off) is a 403 it retries, so
// nobody between them can get fog to unpair it.
func (s *Server) computeNode(w http.ResponseWriter, r *http.Request) *store.ComputeNode {
	if s.compute == nil {
		writeErr(w, http.StatusServiceUnavailable, "compute is not available on this fog")
		return nil
	}
	node := r.Header.Get(compute.HeaderNodeID)
	// From the headers alone: nothing of the body is read for a request
	// that isn't the machine's.
	n, err := s.compute.Registry.AuthenticateRequest(node, r.Header.Get(compute.HeaderNodeNonce),
		r.Header.Get(compute.HeaderNodeTime), r.Header.Get(compute.HeaderNodeAuth), r.Header.Get("Content-Type"),
		r.Header.Get(compute.HeaderBodySHA256))
	refuse := func(status int, code string, msg string) {
		s.writeSignedCompute(w, r, node, status, map[string]string{"error": msg, "code": code, "fogTime": time.Now().UTC().Format(time.RFC3339)})
	}
	switch {
	case err == nil:
		return n
	case errors.Is(err, compute.ErrUnknownMachine):
		refuse(http.StatusUnauthorized, compute.CodeUnknownMachine, "fog has no such machine")
	case errors.Is(err, compute.ErrClock):
		refuse(http.StatusForbidden, compute.CodeClock, "this machine's clock is too far from fog's; set its time right")
	case errors.Is(err, compute.ErrReplayed):
		refuse(http.StatusForbidden, compute.CodeReplay, "a request fog has already taken")
	case errors.Is(err, compute.ErrBadRequestMAC):
		refuse(http.StatusForbidden, compute.CodeBadRequest, "fog couldn't authenticate the request")
	default:
		writeError(w, err)
	}
	return nil
}

// handleComputeJoin pairs a machine with a one-time code.
func (s *Server) handleComputeJoin(w http.ResponseWriter, r *http.Request) {
	if s.compute == nil {
		writeErr(w, http.StatusServiceUnavailable, "compute is not available on this fog")
		return
	}
	if !s.limitStranger(w, r, "compute-join", "too many pairing attempts; wait a few minutes") {
		return
	}
	var req compute.JoinRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Attempts) == 0 {
		// A machine about to pair asks first for the live codes' salts.
		salts, err := s.compute.Registry.Salts()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, compute.JoinSalts{Salts: salts})
		return
	}
	signer, err := s.computeSigner()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ans, err := s.compute.Registry.Join(req, signer.PublicKeyB64())
	switch {
	case errors.Is(err, compute.ErrBadCode):
		forbidden(w, err.Error())
	case errors.Is(err, compute.ErrInvalid):
		badRequest(w, err.Error())
	case err != nil:
		writeError(w, err)
	default:
		s.auditAs("compute-node:"+ans.Node, "compute.pair", req.Inventory.Name, nil)
		writeJSON(w, http.StatusOK, ans)
	}
}

// handleComputePoll is a node's poll: its report in, work out.
func (s *Server) handleComputePoll(w http.ResponseWriter, r *http.Request) {
	n := s.computeNode(w, r)
	if n == nil {
		return
	}
	// A report names at most a machine's runs and their exits; an
	// inventory's filter list is the bulk of it. It must be the body the
	// machine MACed.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil || compute.BodySHA256(body) != r.Header.Get(compute.HeaderBodySHA256) {
		s.writeSignedCompute(w, r, n.ID, http.StatusForbidden, map[string]string{"error": "not the body the machine sent", "code": compute.CodeBadRequest})
		return
	}
	var report compute.NodeReport
	if err := json.Unmarshal(body, &report); err != nil {
		badRequest(w, "invalid JSON body")
		return
	}
	ans, err := s.compute.Poll(r.Context(), n, report, s.clientIP(r))
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeSignedCompute(w, r, n.ID, http.StatusOK, ans)
}

// handleComputeBinary hands out a fog-node build. Public: the installer
// fetches it before the machine is paired, and the binary is no secret.
func (s *Server) handleComputeBinary(w http.ResponseWriter, r *http.Request) {
	if s.compute == nil || s.compute.Binaries == nil {
		notFound(w, "no fog-node builds here")
		return
	}
	platform := strings.TrimSuffix(r.PathValue("platform"), ".sha256")
	b, err := s.compute.Binaries.Get(r.Context(), platform)
	if errors.Is(err, compute.ErrNoBinary) {
		notFound(w, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if strings.HasSuffix(r.PathValue("platform"), ".sha256") {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(b.SHA256 + "\n"))
		return
	}
	f, err := os.Open(b.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Fog-Node-Version", b.Version)
	w.Header().Set("X-Fog-Node-Sha256", b.SHA256)
	http.ServeContent(w, r, "fog-node", info.ModTime(), f)
}

// handleComputeInstall is the script Settings › Compute tells the operator
// to run on the machine: it fetches fog-node from this fog, checks it,
// pairs it with the code, and installs it as a service.
func (s *Server) handleComputeInstall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write([]byte(compute.InstallScript))
}

// computeOverview is Settings › Compute.
type computeOverview struct {
	Machines []compute.Machine `json:"machines"`
	Pools    []compute.Pool    `json:"pools"`
	// Apps is every app that hands fog video work, against every pool.
	Apps []compute.Usage `json:"apps"`
}

func (s *Server) handleComputeOverview(w http.ResponseWriter, r *http.Request) {
	if s.compute == nil {
		writeJSON(w, http.StatusOK, computeOverview{Machines: []compute.Machine{}, Pools: []compute.Pool{}, Apps: []compute.Usage{}})
		return
	}
	machines, err := s.compute.Machines()
	if err != nil {
		writeError(w, err)
		return
	}
	pools := compute.Pools(machines)
	if p := compute.Inference(machines); p != nil {
		pools = append(pools, *p)
	}
	usage, err := s.compute.Usage(pools)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, computeOverview{Machines: machines, Pools: pools, Apps: usage})
}

// handleComputeGrant switches a pool on or off for one app. Off, fog
// withdraws the transcoder from the app on its next pass.
func (s *Server) handleComputeGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		App  string `json:"app"`
		Pool string `json:"pool"`
		On   bool   `json:"on"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	app, err := s.store.AppByName(body.App)
	if err != nil {
		writeError(w, err)
		return
	}
	if (!strings.HasPrefix(body.Pool, "video/") && body.Pool != compute.InferencePool) || len(body.Pool) > 80 {
		badRequest(w, "no such pool")
		return
	}
	if err := s.store.SetComputeGrant(app.ID, body.Pool, body.On); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "compute.grant", app.Name, map[string]any{"pool": body.Pool, "on": body.On})
	writeOK(w)
}

// handleComputeCode makes a pairing code and the command that uses it.
func (s *Server) handleComputeCode(w http.ResponseWriter, r *http.Request) {
	if s.compute == nil {
		writeErr(w, http.StatusServiceUnavailable, "compute is not available on this fog")
		return
	}
	code, err := s.compute.Registry.NewCode(s.actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	base := returnBase(r)
	s.audit(r, "compute.code", "", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"code": code,
		"fog":  base,
		// The code goes in the environment, never on a command line (where
		// any user of the machine could read it and pair first).
		"command":   "curl -fsSL " + base + "/compute/v1/install.sh | FOG_PAIR_CODE=" + code + " sh -s -- " + base,
		"expiresAt": time.Now().Add(compute.CodeTTL).UTC().Format(time.RFC3339),
	})
}

// handleComputeNodeUpdate changes what the operator controls about a
// machine: its name, whether it's paused, and its budget (null for what
// the machine itself says).
func (s *Server) handleComputeNodeUpdate(w http.ResponseWriter, r *http.Request) {
	if s.compute == nil {
		notFound(w, "no such machine")
		return
	}
	id := r.PathValue("id")
	n, err := s.store.ComputeNode(id)
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Name   *string        `json:"name"`
		Paused *bool          `json:"paused"`
		Budget *compute.Slots `json:"budget"`
		Clear  bool           `json:"clearBudget"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name, paused, budget := n.Name, n.Paused, n.Budget
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
		if name == "" || len(name) > 80 {
			badRequest(w, "a machine needs a name of at most 80 characters")
			return
		}
	}
	if body.Paused != nil {
		paused = *body.Paused
	}
	if body.Clear {
		budget = ""
	} else if b := body.Budget; b != nil {
		if b.Interactive < 0 || b.Batch < 0 || b.Interactive > 64 || b.Batch > 64 {
			badRequest(w, "a budget is 0 to 64 of each")
			return
		}
		budget = `{"interactive":` + strconv.Itoa(b.Interactive) + `,"batch":` + strconv.Itoa(b.Batch) + `}`
	}
	if err := s.store.SetComputeNodeSettings(id, name, budget, paused); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "compute.machine.update", name, nil)
	m, err := s.compute.Machine(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleComputeNodeRetryUpdate has a machine behind fog's release try
// updating to it again: Update now.
func (s *Server) handleComputeNodeRetryUpdate(w http.ResponseWriter, r *http.Request) {
	if s.compute == nil {
		notFound(w, "no such machine")
		return
	}
	id := r.PathValue("id")
	n, err := s.store.ComputeNode(id)
	if err != nil {
		writeError(w, err)
		return
	}
	s.compute.RetryUpdate(id)
	s.audit(r, "compute.machine.update-now", n.Name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleComputeNodeRemove forgets a machine.
func (s *Server) handleComputeNodeRemove(w http.ResponseWriter, r *http.Request) {
	if s.compute == nil {
		notFound(w, "no such machine")
		return
	}
	id := r.PathValue("id")
	n, err := s.store.ComputeNode(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.compute.Remove(id); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "compute.machine.remove", n.Name, nil)
	writeOK(w)
}

// handleInference is an app's request to fog's inference pool
// (FOG_INFERENCE_URL), authenticated by its own FOG_API_TOKEN.
func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return
	}
	if s.compute == nil {
		writeErr(w, http.StatusServiceUnavailable, "compute is not available on this fog")
		return
	}
	s.compute.ServeInference(w, r, app)
}

// computeActivityPage is how many rows the activity log answers at once.
const computeActivityPage = 100

// handleComputeActivity is Settings › Compute › Activity: what apps asked of
// compute, newest first, narrowed by app, kind, machine, or to what went
// wrong, and read further back with before.
func (s *Server) handleComputeActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ComputeActivityFilter{App: q.Get("app"), Kind: q.Get("kind"), Machine: q.Get("machine"),
		Problems: q.Get("problems") == "1", Limit: computeActivityPage}
	if b := q.Get("before"); b != "" {
		before, err := strconv.ParseInt(b, 10, 64)
		if err != nil {
			badRequest(w, "before must be an entry id")
			return
		}
		f.Before = before
	}
	rows, err := s.store.ComputeActivityPage(f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
