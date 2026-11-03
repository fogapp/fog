// A person's cryptographic self, as fog sees it: the identity key they wrote
// once, the devices they attested, the KeyPackages those devices published,
// and the backup blob fog stores without being able to open. Every route here
// is person-authed (requirePerson) and every write is the person's own; fog
// verifies signatures and never makes one. docs/chat-contract.md.
package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/people"
	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// DefaultCiphersuite is MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519 — RFC
// 9420's mandatory-to-implement suite and the one both ends speak.
const DefaultCiphersuite = 1

type deviceView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	DeviceKey   string `json:"deviceKey"`
	Attestation string `json:"attestation,omitempty"`
	Verified    bool   `json:"verified"`
	KeyPackages int    `json:"keyPackages"`
	CreatedAt   string `json:"createdAt"`
	LastSeenAt  string `json:"lastSeenAt,omitempty"`
	Current     bool   `json:"current,omitempty"`
}

// deviceView is one device as its owner sees it. keyPackages, when not
// nil, holds every device's unused KeyPackage count, read once for a list.
func (s *Server) deviceView(d store.PersonDevice, currentID int64, keyPackages map[int64]int) deviceView {
	v := deviceView{
		ID: d.ID, Name: d.Name, Kind: d.Kind,
		DeviceKey: people.EncodeKey(d.DeviceKey),
		Verified:  d.Verified(),
		CreatedAt: d.CreatedAt, LastSeenAt: d.LastSeenAt,
		Current: d.ID == currentID && currentID != 0,
	}
	if d.Verified() {
		v.Attestation = people.EncodeKey(d.Attestation)
	}
	if keyPackages != nil {
		v.KeyPackages = keyPackages[d.ID]
	} else {
		v.KeyPackages, _ = s.store.CountKeyPackages(d.ID)
	}
	return v
}

func (s *Server) handleChatMe(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	p := pc.Person
	devs, _ := s.store.PersonDevices(p.ID)
	var currentID int64
	if pc.Session != nil {
		currentID = pc.Session.DeviceID
	}
	kp, _ := s.store.UnusedKeyPackageCounts()
	views := make([]deviceView, 0, len(devs))
	for _, d := range devs {
		views = append(views, s.deviceView(d, currentID, kp))
	}
	var backup any
	if b, err := s.store.PersonBackup(p.ID); err == nil {
		backup = map[string]any{"version": b.Version, "updatedAt": b.UpdatedAt, "bytes": len(b.Blob)}
	}
	identity := ""
	if len(p.IdentityKey) > 0 {
		identity = people.EncodeKey(p.IdentityKey)
	}
	instance := ""
	if s.fed != nil {
		if id, err := s.fed.Identity(); err == nil && id != nil {
			instance = id.InstanceID
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"person":      p,
		"identityKey": identity,
		"devices":     views,
		"deviceId":    currentID,
		"operator":    pc.Operator,
		"household":   map[string]any{"name": s.householdName(), "instanceId": instance, "domain": s.baseDomain()},
		"backup":      backup,
	})
}

// handleSetIdentity is the write-once identity key. A second write is a 409
// unless the operator or the person cleared it through the recovery flow.
func (s *Server) handleSetIdentity(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	var req struct {
		IdentityKey string `json:"identityKey"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key, err := people.DecodeKey(req.IdentityKey)
	if err != nil || people.ValidPublicKey(key) != nil {
		badRequest(w, people.ErrBadKey.Error())
		return
	}
	err = s.store.SetPersonIdentityKey(pc.Person.ID, key)
	if errors.Is(err, store.ErrIdentitySet) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "an identity key is already set for this person", "code": "identity_set"})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "people.identity.set", pc.Person.Handle, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "identityKey": people.EncodeKey(key)})
}

// handleRegisterDevice creates a device for the signed-in person and binds a
// person session to it — the cookie for a browser, a bearer token for a
// native client — so every later call knows which device is speaking.
func (s *Server) handleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	var req struct {
		Name        string   `json:"name"`
		Kind        string   `json:"kind"`
		DeviceKey   string   `json:"deviceKey"`
		Attestation string   `json:"attestation"`
		Ciphersuite int      `json:"ciphersuite"`
		KeyPackages []string `json:"keyPackages"`
		LastResort  string   `json:"lastResort"`
		ClientKind  string   `json:"clientKind"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	clientKind := "web"
	if req.ClientKind == "native" {
		clientKind = "native"
	}
	// Only a browser's own session attaches a device, and a native token
	// only from one freshly proven, within its budgets — all before
	// anything is written (signin_handlers_device.go).
	minter, ok := s.mayAttachDevice(w, r, pc, clientKind, true)
	if !ok {
		return
	}
	if len(req.KeyPackages) > maxKeyPackagesPerRegistration {
		badRequest(w, fmt.Sprintf("at most %d key packages at once", maxKeyPackagesPerRegistration))
		return
	}
	key, err := people.DecodeKey(req.DeviceKey)
	if err != nil || people.ValidPublicKey(key) != nil {
		badRequest(w, people.ErrBadKey.Error())
		return
	}
	var attestation []byte
	if req.Attestation != "" {
		attestation, err = people.DecodeKey(req.Attestation)
		if err != nil {
			badRequest(w, "malformed attestation")
			return
		}
		if len(pc.Person.IdentityKey) == 0 {
			badRequest(w, "set an identity key before attesting a device")
			return
		}
		if err := people.VerifyAttestation(pc.Person.IdentityKey, key, attestation); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	kind := req.Kind
	switch kind {
	case "web", "ios", "android", "desktop":
	default:
		kind = "web"
	}
	name := people.ValidDisplayName(req.Name)
	if name == "" {
		name = deviceNameFromUA(r.UserAgent())
	}
	if _, err := s.store.PersonDeviceByKey(key); err == nil {
		conflict(w, "that device key is already registered")
		return
	}
	id, err := s.store.CreatePersonDevice(pc.Person.ID, name, kind, key, attestation)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(req.KeyPackages) > 0 {
		pkgs, err := decodeKeyPackages(req.KeyPackages)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		cs := req.Ciphersuite
		if cs == 0 {
			cs = DefaultCiphersuite
		}
		if err := s.store.AddKeyPackages(id, cs, pkgs); err != nil {
			writeError(w, err)
			return
		}
	}
	if !s.putLastResort(w, id, req.Ciphersuite, req.LastResort) {
		return
	}
	out := map[string]any{"device": s.deviceView(mustDevice(s, id), id, nil)}
	if !s.attachDevice(w, r, minter, id, clientKind, out) {
		return
	}
	s.audit(r, "people.device.add", pc.Person.Handle, map[string]any{"device": id, "verified": len(attestation) > 0})
	writeJSON(w, http.StatusOK, out)
}

func mustDevice(s *Server, id int64) store.PersonDevice {
	d, err := s.store.PersonDeviceByID(id)
	if err != nil {
		return store.PersonDevice{ID: id}
	}
	return *d
}

func deviceNameFromUA(ua string) string {
	ua = strings.ToLower(ua)
	switch {
	case strings.Contains(ua, "iphone"):
		return "iPhone"
	case strings.Contains(ua, "ipad"):
		return "iPad"
	case strings.Contains(ua, "android"):
		return "Android"
	case strings.Contains(ua, "macintosh"):
		return "Mac"
	case strings.Contains(ua, "windows"):
		return "Windows PC"
	case strings.Contains(ua, "linux"):
		return "Linux"
	}
	return "Browser"
}

func decodeKeyPackages(in []string) ([][]byte, error) {
	if len(in) > people.MaxKeyPackages {
		return nil, fmt.Errorf("at most %d key packages at a time", people.MaxKeyPackages)
	}
	out := make([][]byte, 0, len(in))
	for _, s := range in {
		b, err := people.DecodeKey(s)
		if err != nil || len(b) < 16 || len(b) > 64<<10 {
			return nil, errors.New("malformed key package")
		}
		out = append(out, b)
	}
	return out, nil
}

// putLastResort files a device's last-resort KeyPackage, if one was sent.
// False means it answered with an error.
func (s *Server) putLastResort(w http.ResponseWriter, deviceID int64, cs int, raw string) bool {
	if raw == "" {
		return true
	}
	pkgs, err := decodeKeyPackages([]string{raw})
	if err != nil {
		badRequest(w, "malformed last-resort key package")
		return false
	}
	if cs == 0 {
		cs = DefaultCiphersuite
	}
	if err := s.store.SetLastResortKeyPackage(deviceID, cs, pkgs[0]); err != nil {
		writeError(w, err)
		return false
	}
	return true
}

// ownDevice resolves {id} to one of the person's live devices.
func (s *Server) ownDevice(w http.ResponseWriter, r *http.Request) *store.PersonDevice {
	pc := s.currentPerson(r)
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return nil
	}
	d, err := s.store.PersonDeviceByID(id)
	if err != nil || d.PersonID != pc.Person.ID || d.RevokedAt != "" {
		notFound(w, "no such device")
		return nil
	}
	return d
}

func (s *Server) handleAttestDevice(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	d := s.ownDevice(w, r)
	if d == nil {
		return
	}
	var req struct {
		Attestation string `json:"attestation"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	att, err := people.DecodeKey(req.Attestation)
	if err != nil {
		badRequest(w, "malformed attestation")
		return
	}
	if len(pc.Person.IdentityKey) == 0 {
		badRequest(w, "no identity key set")
		return
	}
	if err := people.VerifyAttestation(pc.Person.IdentityKey, d.DeviceKey, att); err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := s.store.SetDeviceAttestation(d.ID, att); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "people.device.attest", pc.Person.Handle, map[string]any{"device": d.ID})
	writeJSON(w, http.StatusOK, map[string]any{"device": s.deviceView(mustDevice(s, d.ID), 0, nil)})
}

func (s *Server) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	d := s.ownDevice(w, r)
	if d == nil {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := people.ValidDisplayName(req.Name)
	if name == "" {
		badRequest(w, "name required")
		return
	}
	if err := s.store.RenameDevice(d.ID, name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": s.deviceView(mustDevice(s, d.ID), 0, nil)})
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	d := s.ownDevice(w, r)
	if d == nil {
		return
	}
	if err := s.store.RevokeDevice(d.ID); err != nil {
		writeError(w, err)
		return
	}
	s.chat.DeviceRevoked(pc.Person, d.DeviceKey)
	s.audit(r, "people.device.revoke", pc.Person.Handle, map[string]any{"device": d.ID})
	writeOK(w)
}

func (s *Server) handlePublishKeyPackages(w http.ResponseWriter, r *http.Request) {
	d := s.ownDevice(w, r)
	if d == nil {
		return
	}
	var req struct {
		Ciphersuite int      `json:"ciphersuite"`
		KeyPackages []string `json:"keyPackages"`
		LastResort  string   `json:"lastResort"`
		// Replace drops every package the device published before, its
		// last-resort one included, first: a device restored from a backup
		// holds none of their private keys, and a Welcome to one would be
		// sealed to keys nobody has.
		Replace bool `json:"replace"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Replace {
		if req.LastResort == "" {
			badRequest(w, "replacing a device's key packages takes a new last-resort one")
			return
		}
		if err := s.store.DropDeviceKeyPackages(d.ID); err != nil {
			writeError(w, err)
			return
		}
	}
	if !s.putLastResort(w, d.ID, req.Ciphersuite, req.LastResort) {
		return
	}
	pkgs, err := decodeKeyPackages(req.KeyPackages)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	have, _ := s.store.CountKeyPackages(d.ID)
	if have+len(pkgs) > people.MaxKeyPackages {
		badRequest(w, fmt.Sprintf("this device already has %d unused key packages", have))
		return
	}
	cs := req.Ciphersuite
	if cs == 0 {
		cs = DefaultCiphersuite
	}
	if err := s.store.AddKeyPackages(d.ID, cs, pkgs); err != nil {
		writeError(w, err)
		return
	}
	n, _ := s.store.CountKeyPackages(d.ID)
	writeJSON(w, http.StatusOK, map[string]any{"keyPackages": n})
}

// ---- backup ----

func (s *Server) handlePutBackup(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, people.MaxBackupBytes))
	if err != nil {
		badRequest(w, "backup too large or unreadable")
		return
	}
	if len(body) == 0 {
		badRequest(w, "empty backup")
		return
	}
	version, _ := strconv.ParseInt(r.Header.Get("X-Fog-Backup-Version"), 10, 64)
	if cur, err := s.store.PersonBackup(pc.Person.ID); err == nil && version != 0 && version < cur.Version {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a newer backup exists", "version": cur.Version})
		return
	}
	if err := s.store.PutPersonBackup(pc.Person.ID, body, version); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "bytes": len(body)})
}

func (s *Server) handleGetBackup(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	b, err := s.store.PersonBackup(pc.Person.ID)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "no backup")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Fog-Backup-Version", strconv.FormatInt(b.Version, 10))
	w.Header().Set("Last-Modified", b.UpdatedAt)
	w.Write(b.Blob)
}

func (s *Server) handleDeletePersonBackup(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	if err := s.store.DeletePersonBackup(pc.Person.ID); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

// ---- directory and key material ----

type directoryPerson struct {
	Handle      string        `json:"handle"`
	DisplayName string        `json:"displayName"`
	Role        string        `json:"role"`
	IdentityKey string        `json:"identityKey,omitempty"`
	Devices     []directoryDv `json:"devices"`
}

type directoryDv struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	DeviceKey   string `json:"deviceKey"`
	Attestation string `json:"attestation,omitempty"`
	Verified    bool   `json:"verified"`
	KeyPackages int    `json:"keyPackages"`
}

func (s *Server) directoryView(p store.Person, keyPackages map[int64]int) directoryPerson {
	v := directoryPerson{Handle: p.Handle, DisplayName: p.DisplayName, Role: p.Role, Devices: []directoryDv{}}
	if len(p.IdentityKey) > 0 {
		v.IdentityKey = people.EncodeKey(p.IdentityKey)
	}
	devs, _ := s.store.PersonDevices(p.ID)
	for _, d := range devs {
		dv := directoryDv{ID: d.ID, Name: d.Name, Kind: d.Kind, DeviceKey: people.EncodeKey(d.DeviceKey), Verified: d.Verified()}
		if d.Verified() {
			dv.Attestation = people.EncodeKey(d.Attestation)
		}
		dv.KeyPackages = keyPackages[d.ID]
		v.Devices = append(v.Devices, dv)
	}
	return v
}

// handleChatPeople is the local directory: every active person and their
// devices, as a client needs to add them to a room or pin their keys.
func (s *Server) handleChatPeople(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListPeople()
	if err != nil {
		writeError(w, err)
		return
	}
	kp, _ := s.store.UnusedKeyPackageCounts()
	out := []directoryPerson{}
	for _, p := range list {
		if p.Status != store.PersonStatusActive {
			continue
		}
		out = append(out, s.directoryView(p, kp))
	}
	// Peers' people, as their heartbeats advertise them, for the peers we
	// have granted chat.
	type household struct {
		Name       string            `json:"name"`
		InstanceID string            `json:"instanceId"`
		Online     bool              `json:"online"`
		People     []directoryPerson `json:"people"`
	}
	households := []household{}
	granted, _ := s.store.FedCapabilityConfigs(chat.CapChat, store.DirGrant)
	if peers, err := s.store.ListFedPeers(); err == nil {
		for i := range peers {
			peer := &peers[i]
			if _, ok := granted[peer.ID]; !ok {
				continue
			}
			h := household{Name: peer.DisplayName(), InstanceID: peer.InstanceID, Online: fed.Online(peer, time.Now()), People: []directoryPerson{}}
			for _, dp := range chat.PeerPeople(peer) {
				v := directoryPerson{Handle: dp.Handle, DisplayName: dp.DisplayName, Role: dp.Role, IdentityKey: dp.IdentityKey, Devices: []directoryDv{}}
				for _, d := range dp.Devices {
					v.Devices = append(v.Devices, directoryDv{ID: d.ID, Name: d.Name, Kind: d.Kind, DeviceKey: d.DeviceKey, Attestation: d.Attestation, Verified: d.Verified})
				}
				h.People = append(h.People, v)
			}
			households = append(households, h)
		}
	}
	inst, _ := s.fed.Identity()
	instanceID := ""
	if inst != nil {
		instanceID = inst.InstanceID
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": out, "household": s.householdName(), "instanceId": instanceID, "households": households})
}

// handleChatPersonKeys is MIMI's keyMaterial: the person's identity key and,
// per live device, its key, attestation, and one fresh KeyPackage. Each call
// consumes one package per device; a device with none left reports null so
// the caller can say "that device will join when it publishes keys".
func (s *Server) handleChatPersonKeys(w http.ResponseWriter, r *http.Request) {
	handle, err := people.LookupHandle(r.PathValue("handle"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	p, err := s.store.PersonByHandle(handle)
	if err != nil || p.Status != store.PersonStatusActive {
		notFound(w, "no such person")
		return
	}
	cs, _ := strconv.Atoi(r.URL.Query().Get("ciphersuite"))
	if cs == 0 {
		cs = DefaultCiphersuite
	}
	dev, _ := strconv.ParseInt(r.URL.Query().Get("device"), 10, 64)
	writeJSON(w, http.StatusOK, s.keyMaterialFor(p, cs, dev, s.mayConsumeKeys(s.currentPerson(r).Person, p)))
}

// mayConsumeKeys spends asker's budget for one fetch of target's one-use
// packages. A person fetching their own (sibling sync) is not bounded.
func (s *Server) mayConsumeKeys(asker, target *store.Person) bool {
	if asker != nil && asker.ID == target.ID {
		return true
	}
	var id int64
	if asker != nil {
		id = asker.ID
	}
	return s.chat.Keys.Take(time.Now(), chat.LocalRules(id, target.Handle)...)
}

// ---- binding a session to a device the person already holds ----

// handleDeviceChallenge hands out a nonce a device signs to prove it holds
// the key fog has on file — how a restored or re-signed-in device gets a
// device-bound session without registering a new key.
func (s *Server) handleDeviceChallenge(w http.ResponseWriter, r *http.Request) {
	d := s.ownDevice(w, r)
	if d == nil {
		return
	}
	nonce := randx.Token()
	s.deviceNonces.Put(d.ID, nonce)
	writeJSON(w, http.StatusOK, map[string]any{"nonce": nonce, "message": people.DeviceSessionMessage(nonce)})
}

func (s *Server) handleDeviceSession(w http.ResponseWriter, r *http.Request) {
	pc := s.currentPerson(r)
	d := s.ownDevice(w, r)
	if d == nil {
		return
	}
	var req struct {
		Nonce      string `json:"nonce"`
		Signature  string `json:"signature"`
		ClientKind string `json:"clientKind"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	clientKind := "web"
	if req.ClientKind == "native" {
		clientKind = "native"
	}
	minter, ok := s.mayAttachDevice(w, r, pc, clientKind, false) // signin_handlers_device.go
	if !ok {
		return
	}
	if !s.deviceNonces.Take(d.ID, req.Nonce) {
		badRequest(w, "challenge expired; ask for another")
		return
	}
	sig, err := people.DecodeKey(req.Signature)
	if err != nil || !people.VerifyDeviceSession(d.DeviceKey, req.Nonce, sig) {
		forbidden(w, "the signature does not match this device's key")
		return
	}
	out := map[string]any{"device": s.deviceView(*d, d.ID, nil)}
	if !s.attachDevice(w, r, minter, d.ID, clientKind, out) {
		return
	}
	s.audit(r, "people.device.session", pc.Person.Handle, map[string]any{"device": d.ID})
	writeJSON(w, http.StatusOK, out)
}

type keyMaterialDevice struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	DeviceKey   string  `json:"deviceKey"`
	Attestation string  `json:"attestation,omitempty"`
	Verified    bool    `json:"verified"`
	KeyPackage  *string `json:"keyPackage"`
	// LastResort says KeyPackage is the device's last-resort package,
	// served again to whoever asks: the device had no one-use package left,
	// or the asker had used up its budget.
	LastResort bool `json:"lastResort,omitempty"`
}

type keyMaterial struct {
	Handle      string              `json:"handle"`
	DisplayName string              `json:"displayName"`
	IdentityKey string              `json:"identityKey,omitempty"`
	Ciphersuite int                 `json:"ciphersuite"`
	Devices     []keyMaterialDevice `json:"devices"`
}

// keyMaterialFor narrows key material to one device when onlyDevice is set —
// a person adding their own new device consumes only that device's package.
// consume says the asker is within its budget (chat.KeyBudget) and takes
// one-use packages; otherwise, and for a device with none left, each device
// offers its last-resort package, which is never used up.
func (s *Server) keyMaterialFor(p *store.Person, cs int, onlyDevice int64, consume bool) keyMaterial {
	km := keyMaterial{Handle: p.Handle, DisplayName: p.DisplayName, Ciphersuite: cs, Devices: []keyMaterialDevice{}}
	if len(p.IdentityKey) > 0 {
		km.IdentityKey = people.EncodeKey(p.IdentityKey)
	}
	devs, _ := s.store.PersonDevices(p.ID)
	for _, d := range devs {
		if onlyDevice != 0 && d.ID != onlyDevice {
			continue
		}
		dv := keyMaterialDevice{ID: d.ID, Name: d.Name, Kind: d.Kind, DeviceKey: people.EncodeKey(d.DeviceKey), Verified: d.Verified()}
		if d.Verified() {
			dv.Attestation = people.EncodeKey(d.Attestation)
		}
		var kp []byte
		if consume {
			kp, _ = s.store.ConsumeKeyPackage(d.ID, cs)
		}
		if kp == nil {
			if lr, err := s.store.LastResortKeyPackage(d.ID, cs); err == nil {
				kp, dv.LastResort = lr, true
			}
		}
		if kp != nil {
			enc := people.EncodeKey(kp)
			dv.KeyPackage = &enc
		}
		km.Devices = append(km.Devices, dv)
	}
	return km
}
