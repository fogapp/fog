package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"gitea.lan/matthew/fog/internal/dockerx"
	"net"
	"net/http"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/apps"
	"gitea.lan/matthew/fog/internal/dnssync"
	"gitea.lan/matthew/fog/internal/proxy"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/wan"
)

// The domain wizard: Cloudflare-native HTTPS in four staged calls
// (check → dns → apply → verify), each returning ✓/✗ facts with fixes.

// portFree reports whether Fog could own a port — free, or already held by
// Fog's own Caddy container.
func (s *Server) portFree(ctx context.Context, port string) (bool, string) {
	l, err := net.Listen("tcp", ":"+port)
	if err == nil {
		l.Close()
		return true, ""
	}
	if s.proxyRunning(ctx) {
		return true, "held by Fog's proxy"
	}
	return false, err.Error()
}

// proxyRunning reports whether Fog's Caddy container is up.
func (s *Server) proxyRunning(ctx context.Context) bool {
	cs, err := s.docker.ListByLabel(ctx, "fog.system=caddy")
	if err != nil {
		return false
	}
	for _, c := range cs {
		// By name: the label is one any image can carry.
		if c.State == "running" && dockerx.FogSystem(c) == "caddy" {
			return true
		}
	}
	return false
}

// handleDomainStatus reports the wizard's true current state so the page
// initializes from reality instead of re-running completed steps.
func (s *Server) handleDomainStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	_, tokErr := s.store.GetSecret(0, proxy.SettingDNSToken)
	desired, target := s.keeper().DesiredIP()
	res := map[string]any{
		"domain":     domain,
		"tokenSaved": tokErr == nil,
		"serverIp":   wan.LANIP(),
		"dnsTarget":  target,
		// What the record should hold; differs from serverIp with the wan
		// target, and is empty while the public address is still unknown.
		"desiredIp": desired,
	}
	proxyRunning := s.proxyRunning(ctx)
	res["proxyRunning"] = proxyRunning
	if domain == "" {
		writeJSON(w, http.StatusOK, res)
		return
	}

	// Wildcard record state (best-effort — network hiccups mean "unknown").
	// The record's *content* is compared against the server's current IP:
	// a record that exists but points at an address this machine no longer
	// has (the move-to-a-new-LAN case) is "stale", never "ok" — reporting it
	// as fine is how an outage hides behind a green checkmark.
	res["wildcardRecord"] = "unknown"
	if tokErr == nil {
		if cf, err := s.keeper().CloudflareClient(); err == nil {
			if zone, err := cf.FindZone(ctx, domain); err == nil {
				res["wildcardRecord"] = "missing"
				if records, err := cf.Records(ctx, zone.ID); err == nil {
					for _, rec := range records {
						if rec.Name == "*."+domain && rec.Type == "A" && !rec.Proxied {
							if desired != "" && rec.Content == desired {
								res["wildcardRecord"] = "ok"
							} else {
								res["wildcardRecord"] = "stale"
								res["wildcardRecordIp"] = rec.Content
							}
						}
					}
				}
			}
		}
	}

	if proxyRunning {
		host := "fog." + domain
		if ips, err := net.DefaultResolver.LookupHost(ctx, host); err == nil && len(ips) > 0 {
			res["resolvedIps"] = ips
			client := &http.Client{Timeout: 2 * time.Second}
			if resp, err := client.Get("https://" + host + "/api/setup/status"); err == nil {
				resp.Body.Close()
				res["httpsOk"] = true
				res["fqdnApps"] = s.fqdnTemplatedApps()
			}
		}
	}
	writeJSON(w, http.StatusOK, res)
}

type domainCheckReq struct {
	Domain string `json:"domain"`
	Token  string `json:"token"`
}

func (s *Server) handleDomainCheck(w http.ResponseWriter, r *http.Request) {
	var req domainCheckReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	if !validDomain(req.Domain) {
		badRequest(w, "enter a bare domain like example.com")
		return
	}
	actor := s.actor(r)
	// Persist inputs directly (no proxy-apply side effects mid-wizard). A
	// failed write must fail the check — a wizard that reports on a domain it
	// never saved would pass here and break on the next step.
	if err := s.store.SetSetting(store.SettingBaseDomain, req.Domain); err != nil {
		writeError(w, err)
		return
	}
	if tok := strings.TrimSpace(req.Token); tok != "" {
		if err := s.secrets().Put(0, proxy.SettingDNSToken, tok); err != nil {
			writeError(w, err)
			return
		}
	}
	s.auditAs(actor, "domain.check", req.Domain, nil)

	res := map[string]any{"domain": req.Domain, "serverIp": wan.LANIP()}
	cf, err := s.keeper().CloudflareClient()
	if err != nil {
		res["tokenValid"] = false
		res["tokenError"] = err.Error()
		writeJSON(w, http.StatusOK, res)
		return
	}
	ctx := r.Context()
	if err := cf.VerifyToken(ctx); err != nil {
		res["tokenValid"] = false
		res["tokenError"] = err.Error()
		writeJSON(w, http.StatusOK, res)
		return
	}
	res["tokenValid"] = true

	zone, err := cf.FindZone(ctx, req.Domain)
	if err != nil {
		res["zoneFound"] = false
		res["zoneError"] = err.Error()
		writeJSON(w, http.StatusOK, res)
		return
	}
	res["zoneFound"] = true
	res["zoneActive"] = zone.Status == "active"
	res["nameServers"] = zone.NameServers

	// Existing records that would shadow Fog hostnames (a specific record
	// always beats the wildcard).
	conflicts := []string{}
	if records, err := cf.Records(ctx, zone.ID); err == nil {
		names := map[string]bool{"fog." + req.Domain: true}
		if list, err := apps.WithSpecs(s.store); err == nil {
			for _, a := range list {
				spec := a.Spec
				if spec == nil {
					// Can't read the spec — fall back to the default name so
					// the app is still checked for a shadowing record.
					names[a.Name+"."+req.Domain] = true
					continue
				}
				if len(spec.Routes) == 0 {
					continue
				}
				for _, r := range spec.Routes {
					if h := apps.RouteHost(a.Name, r, req.Domain); h != "" {
						names[h] = true
					}
				}
			}
		}
		for _, rec := range records {
			if names[rec.Name] {
				conflicts = append(conflicts, fmt.Sprintf("%s (%s → %s) — this existing record will shadow Fog's %s; delete or rename it in Cloudflare", rec.Name, rec.Type, rec.Content, rec.Name))
			}
		}
	}
	res["conflicts"] = conflicts

	p80, why80 := s.portFree(ctx, "80")
	p443, why443 := s.portFree(ctx, "443")
	res["port80"] = map[string]any{"ok": p80, "detail": why80}
	res["port443"] = map[string]any{"ok": p443, "detail": why443}
	writeJSON(w, http.StatusOK, res)
}

// handleDomainDNS is the wizard's "create the record" step. It runs the
// same reconcile the keeper runs unattended, so the record lands on
// whichever address the target calls for and the keeper's own state
// (confirmed address, port mapping) is in step from the first click.
func (s *Server) handleDomainDNS(w http.ResponseWriter, r *http.Request) {
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	if _, err := s.keeper().CloudflareClient(); err != nil {
		badRequest(w, err.Error())
		return
	}
	if domain == "" || wan.LANIP() == "" {
		badRequest(w, "run the check step first")
		return
	}
	st := s.keeper().Reconcile(r.Context(), true)
	if st.RecordIP == "" || st.LastError != "" || st.RecordIP != st.DesiredIP {
		why := st.LastError
		if why == "" {
			why = st.PublicIPError
		}
		if why == "" {
			why = "the record could not be confirmed"
		}
		badRequest(w, "creating the DNS record failed: "+why)
		return
	}
	s.audit(r, "domain.dns", "*."+domain+" → "+st.RecordIP, map[string]any{"target": string(st.Target)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": st.LastAction, "record": "*." + domain + " → " + st.RecordIP, "reach": st})
}

// handleDomainRefresh is the Refresh button: re-learn the address, rewrite
// the record if it drifted, renew the router's port mapping. Returns the
// keeper's snapshot so the page can show exactly what just happened.
func (s *Server) handleDomainRefresh(w http.ResponseWriter, r *http.Request) {
	st := s.keeper().Reconcile(r.Context(), true)
	s.audit(r, "domain.dns.refresh", st.RecordIP, map[string]any{"target": string(st.Target)})
	writeJSON(w, http.StatusOK, s.reachResponse(st))
}

// reachResponse is the keeper's snapshot plus the beacon URL, which is
// derived (domain + token) rather than stored on the snapshot.
func (s *Server) reachResponse(st dnssync.Status) map[string]any {
	res := map[string]any{"status": st}
	if url, err := s.keeper().BeaconURL(); err == nil {
		res["beaconUrl"] = url
	}
	return res
}

func (s *Server) handleReachStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reachResponse(s.keeper().Status()))
}

type reachUpdateReq struct {
	Target  string `json:"target"`            // "lan" | "wan"
	PortMap *bool  `json:"portMap,omitempty"` // ask the router to open ports
}

// handleReachUpdate is the checkbox. Saving the target and reconciling
// happen in one request so the answer the page gets back is the state the
// change actually produced — not the state before it.
func (s *Server) handleReachUpdate(w http.ResponseWriter, r *http.Request) {
	var req reachUpdateReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Target != "lan" && req.Target != "wan" {
		badRequest(w, `target must be "lan" or "wan"`)
		return
	}
	k := s.keeper()
	prev, _ := s.store.GetSetting(dnssync.SettingTarget)
	if err := s.store.SetSetting(dnssync.SettingTarget, req.Target); err != nil {
		writeError(w, err)
		return
	}
	if req.PortMap != nil {
		v := ""
		if !*req.PortMap {
			v = "off"
		}
		if err := s.store.SetSetting(dnssync.SettingPortMap, v); err != nil {
			writeError(w, err)
			return
		}
	}
	if dnssync.ParseTarget(prev) != dnssync.ParseTarget(req.Target) {
		// Proof of reach from the old configuration says nothing about
		// the new one.
		k.ClearBeacon()
	}
	if _, err := k.BeaconToken(); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "domain.reach", req.Target, nil)
	st := k.Reconcile(r.Context(), true)
	writeJSON(w, http.StatusOK, s.reachResponse(st))
}

// handleReachBeacon is the page a phone lands on from outside. It records
// the visit (address, and whether that address is a public one) so the
// Network page can say "reached from outside at 12:04", and answers with
// a plain page rather than JSON — the visitor is a person.
func (s *Server) handleReachBeacon(w http.ResponseWriter, r *http.Request) {
	k := s.keeper()
	if !k.CheckBeacon(r.PathValue("token")) {
		http.NotFound(w, r)
		return
	}
	seen := k.RecordBeacon(s.clientIP(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	where := "from outside your home network"
	if !seen.Public {
		where = "from inside your home network — turn Wi-Fi off and open this link again to test from outside"
	}
	fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Fog reached</title>
<body style="font-family:system-ui,sans-serif;background:#0f1216;color:#e6e9ee;display:grid;place-items:center;min-height:100vh;margin:0">
<main style="max-width:28rem;padding:2rem;text-align:center">
<p style="font-size:3rem;margin:0">✓</p>
<h1 style="font-size:1.25rem;font-weight:600;margin:.5rem 0">Fog is reachable</h1>
<p style="color:#9aa3ad;margin:0">This request arrived %s (seen as %s). The Network page on your server has been told.</p>
</main></body>`, where, seen.IP)
}

func (s *Server) handleDomainApply(w http.ResponseWriter, r *http.Request) {
	if err := s.proxy.Apply(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	s.audit(r, "domain.apply", "", nil)
	writeOK(w)
}

// handleDomainVerify proves the whole chain: LAN DNS resolves the name to
// this server (detecting router rebind protection), and HTTPS answers with a
// real certificate. Retries briefly — cert issuance takes a moment.
func (s *Server) handleDomainVerify(w http.ResponseWriter, r *http.Request) {
	domain, _ := s.store.GetSetting(store.SettingBaseDomain)
	if domain == "" {
		badRequest(w, "no domain configured")
		return
	}
	host := "fog." + domain
	expected, target := s.keeper().DesiredIP()
	res := map[string]any{"host": host, "expectedIp": expected, "dnsTarget": target}

	// Single-shot and fast — the UI polls this until everything is green.
	ips, err := net.DefaultResolver.LookupHost(r.Context(), host)
	res["resolvedIps"] = ips
	resolved := err == nil && len(ips) > 0
	res["resolves"] = resolved
	if !resolved {
		res["rebindSuspected"] = true
		res["hint"] = "If this persists more than a couple of minutes, your router's DNS-rebind protection may be dropping answers that contain private IPs — allowlist " + domain + " in the router/Pi-hole settings."
		writeJSON(w, http.StatusOK, res)
		return
	}

	client := &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: host},
			// Trust DNS for routing but verify the certificate normally.
		},
	}
	resp, herr := client.Get("https://" + host + "/api/setup/status")
	if herr == nil {
		resp.Body.Close()
		res["httpsOk"] = true
		res["fqdnApps"] = s.fqdnTemplatedApps()
	} else {
		res["httpsOk"] = false
		res["httpsError"] = herr.Error()
		res["hint"] = "The certificate is usually issuing — this resolves itself within a few minutes on first run."
		if target == dnssync.TargetWAN {
			// The name now resolves to the public address, so this request
			// left for the router and had to come back in. Routers that
			// don't do that (no "NAT loopback"/"hairpin") fail exactly here
			// while the outside world is fine — say so, or the operator
			// chases a certificate problem that isn't one.
			res["hairpinSuspected"] = true
			res["hint"] = "From inside the house this name now points at your public address, and the router has to loop that traffic back in (\"NAT loopback\" or \"hairpin NAT\"). " +
				"If the beacon link confirms the outside works but this keeps failing, enable NAT loopback on the router, or give " + domain + " a local DNS override to " + wan.LANIP() + " in the router/Pi-hole."
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// fqdnTemplatedApps lists apps whose env stamps {{fqdn}} at deploy — they
// need a redeploy to learn the new hostname.
func (s *Server) fqdnTemplatedApps() []string {
	out := []string{}
	list, err := apps.WithSpecs(s.store)
	if err != nil {
		return out
	}
	for _, a := range list {
		spec := a.Spec
		if spec == nil {
			continue
		}
		for _, v := range spec.Env {
			if strings.Contains(v, "{{fqdn}}") {
				out = append(out, a.Name)
				break
			}
		}
	}
	return out
}
