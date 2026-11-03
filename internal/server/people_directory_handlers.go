// The people directory as apps see it (docs/signin-contract.md):
// the household, and — for apps that federate — the people paired
// households advertise, each by the person address that outlives a rename.
// Read by apps that declare fog-signin/1, with their FOG_API_TOKEN.
package server

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/chat"
	"gitea.lan/matthew/fog/internal/identity"
	"gitea.lan/matthew/fog/internal/store"
)

type directoryHome struct {
	Instance string `json:"instance"`
	Name     string `json:"name"`
	Ours     bool   `json:"ours"`
}

// directoryEntry is one person, as an app may know them.
type directoryEntry struct {
	Address     string             `json:"address"`
	Handle      string             `json:"handle"`
	DisplayName string             `json:"displayName"`
	Home        directoryHome      `json:"home"`
	Status      string             `json:"status"`
	Email       string             `json:"email,omitempty"`
	Chat        string             `json:"chat,omitempty"`
	Pages       []store.PersonPage `json:"pages"`
}

// directoryApp authenticates an app for the directory: it must declare
// fog-signin/1 (the directory is part of that contract), and it sees peers'
// people only if it federates.
func (s *Server) directoryApp(w http.ResponseWriter, r *http.Request) (*store.App, *store.OIDCClient, bool, bool) {
	app, ok := s.appAuthed(w, r)
	if !ok {
		return nil, nil, false, false
	}
	client, err := s.store.OIDCClientForApp(app.ID)
	if err != nil {
		forbidden(w, app.Name+" does not declare fog-signin/1")
		return nil, nil, false, false
	}
	contracts, _ := s.store.AppContracts(app.ID)
	return app, client, slices.Contains(contracts, ContractConnectV1), true
}

// directoryEntries assembles everyone an app may see, without their pages:
// a caller after one person attaches pages to that one alone.
func (s *Server) directoryEntries(client *store.OIDCClient, withPeers bool) ([]directoryEntry, error) {
	inst, err := s.fed.Identity()
	if err != nil {
		return nil, err
	}
	iss := s.issuer()
	chatLink := func(addr string) string {
		if iss == "" {
			return ""
		}
		return iss + "/chat/with/" + url.PathEscape(addr)
	}
	email := slices.Contains(client.Scopes, "email")
	ours := directoryHome{Instance: inst.InstanceID, Name: householdNameOf(s.store), Ours: true}
	list, err := s.store.ListPeople()
	if err != nil {
		return nil, err
	}
	out := []directoryEntry{}
	for _, p := range list {
		if p.Role != store.PersonRoleOperator && p.Role != store.PersonRoleMember {
			continue // guests and bots are not part of an app's household
		}
		addr := identity.Address(inst.InstanceID, p.UID)
		e := directoryEntry{Address: addr, Handle: p.Handle, DisplayName: displayOr(&p), Home: ours, Status: p.Status, Chat: chatLink(addr)}
		if email {
			e.Email = p.Email
		}
		out = append(out, e)
	}
	if withPeers {
		peers, err := s.store.ListFedPeers()
		if err != nil {
			return nil, err
		}
		for _, peer := range peers {
			home := directoryHome{Instance: peer.InstanceID, Name: peer.DisplayName()}
			for _, dp := range chat.PeerPeople(&peer) {
				// Only the people a peer gave an address — the ones its fog
				// says outlive a rename — and only under that peer's own
				// instance: a peer speaks for its own people and nobody else.
				if dp.Address == "" || !strings.HasPrefix(dp.Address, "person:"+peer.InstanceID+"/") {
					continue
				}
				name := dp.DisplayName
				if name == "" {
					name = dp.Handle
				}
				out = append(out, directoryEntry{Address: dp.Address, Handle: dp.Handle, DisplayName: name, Home: home,
					Status: store.PersonStatusActive, Chat: chatLink(dp.Address)})
			}
		}
	}
	return out, nil
}

// directory is directoryEntries with every entry's pages, read in one query.
func (s *Server) directory(client *store.OIDCClient, withPeers bool) ([]directoryEntry, error) {
	out, err := s.directoryEntries(client, withPeers)
	if err != nil {
		return nil, err
	}
	pages, err := s.store.AllPersonPages()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Pages = pages[out[i].Address]
		if out[i].Pages == nil {
			out[i].Pages = []store.PersonPage{}
		}
	}
	return out, nil
}

func (s *Server) handleAppPeople(w http.ResponseWriter, r *http.Request) {
	_, client, withPeers, ok := s.directoryApp(w, r)
	if !ok {
		return
	}
	list, err := s.directory(client, withPeers)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": list})
}

func (s *Server) handleAppPerson(w http.ResponseWriter, r *http.Request) {
	_, client, withPeers, ok := s.directoryApp(w, r)
	if !ok {
		return
	}
	addr := r.PathValue("address")
	list, err := s.directoryEntries(client, withPeers)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, e := range list {
		if e.Address == addr {
			if e.Pages, err = s.store.PersonPages(addr); err != nil {
				writeError(w, err)
				return
			}
			if e.Pages == nil {
				e.Pages = []store.PersonPage{}
			}
			writeJSON(w, http.StatusOK, e)
			return
		}
	}
	notFound(w, "no such person in this app's directory")
}

// handleAppPersonPage registers (or, with an empty url, removes) where a
// person lives in the calling app. The page must be on one of the app's own
// hosts over https: fog chat links to it, and a link chat shows must never
// lead anywhere the app does not serve.
func (s *Server) handleAppPersonPage(w http.ResponseWriter, r *http.Request) {
	app, client, withPeers, ok := s.directoryApp(w, r)
	if !ok {
		return
	}
	var req struct {
		Address string `json:"address"`
		URL     string `json:"url"`
		Label   string `json:"label"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	known, err := s.directoryEntries(client, withPeers)
	if err != nil {
		writeError(w, err)
		return
	}
	if !slices.ContainsFunc(known, func(e directoryEntry) bool { return e.Address == req.Address }) {
		badRequest(w, "no such person in this app's directory")
		return
	}
	if req.URL == "" {
		if err := s.store.DeletePersonPage(app.ID, req.Address); err != nil {
			writeError(w, err)
			return
		}
		writeOK(w)
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme != "https" || !s.appServesHost(app, u.Host) || u.User != nil {
		badRequest(w, "the page must be an https URL on one of "+app.Name+"'s own hosts")
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "Open in " + app.Name
	}
	if len(label) > 80 {
		label = label[:80]
	}
	if err := s.store.PutPersonPage(app.ID, req.Address, u.String(), label); err != nil {
		writeError(w, err)
		return
	}
	writeOK(w)
}

// appServesHost reports whether host is one of an app's routes.
func (s *Server) appServesHost(app *store.App, host string) bool {
	spec, err := apps.LatestSpec(s.store, app.ID)
	if err != nil {
		return false
	}
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	for _, rt := range spec.Routes {
		if h := apps.RouteHost(app.Name, rt, domain); h != "" && strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// handlePersonLookup is chat's side of a person address: who it names, the
// chat address to reach them at, and the pages apps registered for them —
// what /chat/with/<address> and the profile pane's "Open in Hub" need. Any
// signed-in person may ask: it reveals no more than the directory they can
// already see in chat.
func (s *Server) handlePersonLookup(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("address")
	inst, err := s.fed.Identity()
	if err != nil {
		writeError(w, err)
		return
	}
	// A chat address (fog:<instance>/<handle>) names the same person by
	// handle: find their person address first.
	if chatAddr := r.URL.Query().Get("chat"); chatAddr != "" {
		addr = s.personAddressForChat(inst.InstanceID, chatAddr)
		if addr == "" {
			notFound(w, "no person address is known for "+chatAddr)
			return
		}
	}
	out := map[string]any{"address": addr}
	rest, ok := strings.CutPrefix(addr, "person:")
	home, who, _ := strings.Cut(rest, "/")
	if !ok || home == "" || who == "" {
		badRequest(w, "not a person address")
		return
	}
	handle := ""
	if home == inst.InstanceID {
		p, err := s.store.PersonByUID(who)
		if err != nil || p.Status != store.PersonStatusActive {
			notFound(w, "no such person")
			return
		}
		handle, out["displayName"], out["ours"] = p.Handle, displayOr(p), true
	} else {
		peer, err := s.store.FedPeerByInstanceID(home)
		if err != nil {
			notFound(w, "that household is not paired with this one")
			return
		}
		for _, dp := range chat.PeerPeople(peer) {
			if dp.Address == addr {
				handle, out["displayName"], out["household"] = dp.Handle, dp.DisplayName, peer.DisplayName()
			}
		}
		if handle == "" {
			notFound(w, "that household does not share this person with you")
			return
		}
	}
	out["handle"] = handle
	chatAddr := "fog:" + home + "/" + handle
	out["chatAddress"] = chatAddr
	// The rooms the asker shares with them: what "Message Dave" opens.
	shared := []string{}
	if pc, ok := s.resolvePerson(r); ok {
		if mine, err := s.chat.Addr(pc.Person.Handle); err == nil {
			if rooms, err := s.store.SharedChatRooms(mine, chatAddr); err == nil && rooms != nil {
				shared = rooms
			}
		}
	}
	rooms := []map[string]any{}
	for _, id := range shared {
		if room, err := s.store.ChatRoom(id); err == nil {
			rooms = append(rooms, map[string]any{"id": room.ID, "kind": room.Kind, "label": room.Label})
		}
	}
	out["sharedRooms"] = rooms
	pages, err := s.store.PersonPages(addr)
	if err != nil {
		writeError(w, err)
		return
	}
	if pages == nil {
		pages = []store.PersonPage{}
	}
	out["pages"] = pages
	writeJSON(w, http.StatusOK, out)
}

// personAddressForChat maps a chat address to the person address of the
// same person, through our people or a peer's advertised directory.
func (s *Server) personAddressForChat(instance, chatAddr string) string {
	home, handle, ok := chat.ParseAddr(chatAddr)
	if !ok {
		return ""
	}
	if home == instance {
		p, err := s.store.PersonByHandle(handle)
		if err != nil {
			return ""
		}
		return identity.Address(instance, p.UID)
	}
	peer, err := s.store.FedPeerByInstanceID(home)
	if err != nil {
		return ""
	}
	for _, dp := range chat.PeerPeople(peer) {
		if dp.Handle == handle && strings.HasPrefix(dp.Address, "person:"+home+"/") {
			return dp.Address
		}
	}
	return ""
}
