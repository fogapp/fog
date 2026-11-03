// The federation app directory: what this instance's heartbeats advertise
// to each peer about the apps it runs (docs/federation-contract.md).
// Discovery data only — a directory entry hands a peer's apps a URL and a
// name, never any access.
package apps

import (
	"gitea.lan/matthew/fog/internal/fed"
	"gitea.lan/matthew/fog/internal/store"
)

// DirectoryFor builds the directory published to one peer: every app the
// peer's apps.directory grant covers, addressed by its public URL when one
// resolves. No grant → nil. The grant's scope is exactly its config (see
// fed.AppScope): {} publishes every app, {"apps": [...]} those named and no
// others — an empty list publishes none. A config that doesn't read as one
// of those publishes nothing: a scope that can't be read is not widened.
//
// A missing base domain is not a reason to publish nothing: a route with a
// fully-qualified host resolves without one (RouteHost), and an app that
// declares a contract is worth advertising even with no URL at all — the
// peer's app can then say "no public URL" instead of "no such app", and a
// dial-in-only counterpart can still be connected to. Apps with neither a URL
// nor a contract carry no information and are skipped.
func (e *Engine) DirectoryFor(peer *store.FedPeer) []fed.DirectoryApp {
	raw, ok, err := e.Store.FedCapabilityConfig(peer.ID, store.CapAppsDirectory, store.DirGrant)
	if err != nil || !ok {
		return nil
	}
	scope, err := fed.ParseAppScope(raw)
	if err != nil {
		return nil
	}
	domain, _ := e.Store.GetSetting(store.SettingBaseDomain)
	// The one answer to "what does this app currently say it is" — see
	// LatestSpec; decoding revisions by hand here is exactly the per-caller
	// drift it exists to prevent.
	list, err := WithSpecs(e.Store)
	if err != nil {
		return nil
	}
	allContracts, err := e.Store.AllAppContracts()
	if err != nil {
		return nil
	}
	// An app held for review isn't serving; peers hear of it once it is
	// confirmed.
	held, _ := e.Store.HeldApps()
	var out []fed.DirectoryApp
	for _, a := range list {
		if !scope.Covers(a.Name) || held[a.Name] {
			continue
		}
		spec := a.Spec
		if spec == nil || len(spec.Routes) == 0 {
			continue
		}
		host := RouteHost(a.Name, spec.Routes[0], domain)
		contracts := allContracts[a.ID]
		if host == "" && len(contracts) == 0 {
			continue
		}
		url := ""
		if host != "" {
			url = "https://" + host
		}
		out = append(out, fed.DirectoryApp{
			App:       a.Name,
			URL:       url,
			Contracts: contracts,
		})
	}
	return out
}
