package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"gitea.lan/matthew/fog/internal/appcall"
	"gitea.lan/matthew/fog/internal/integrations"
	"gitea.lan/matthew/fog/internal/metrics"
	"gitea.lan/matthew/fog/internal/store"
)

// fogd reports to Glass as the app "fog" when Glass is installed. It asks
// for its own ingest key through the glass/monitoring offer, as connecting
// any app on its Integrations tab would — Glass makes the "Fog" app on its
// side — and keeps the key in the vault. Revoking that key in Glass ›
// Settings › Apps is the off switch, and it holds: fogd mints again only
// when Glass no longer knows the integration at all (a fresh install).
const (
	glassProvider = "glass"
	glassConsumer = "fog"
	glassOffer    = "monitoring"

	settingGlassIntegration = "glass_integration"
	settingGlassKey         = "glass_ingest_key" // vault-encrypted
)

// errGlassForgot: Glass answered 404 for the integration — it is not the
// Glass that minted it.
var errGlassForgot = errors.New("glass does not know this integration")

func glassRefusal(resp *http.Response, raw []byte) error {
	if resp.StatusCode == http.StatusNotFound && resp.Request.Method != http.MethodPost {
		return errGlassForgot
	}
	return integrations.Refusal(resp, raw)
}

// keepGlass is one pass of the telemetry connection: point the client at
// Glass's address on the fog network as it is now (a redeploy moves it),
// minting a key first if fogd has none. Without a running Glass the client
// goes inert.
func (d *daemon) keepGlass(ctx context.Context) {
	app, err := d.st.AppByName(glassProvider)
	if err != nil || !integrations.Running(app) {
		d.telemetry.Set("", "")
		return
	}
	ep := d.engine.AppEndpoints(ctx, []store.App{*app})[0]
	if ep.Err != nil {
		d.telemetry.Set("", "")
		return
	}
	call := appcall.New(d.engine.AppEndpoints).Refusing(glassRefusal)

	id, _ := d.st.GetSetting(settingGlassIntegration)
	key := ""
	if enc, _ := d.st.GetSetting(settingGlassKey); enc != "" {
		key, _ = d.vlt.Decrypt(enc)
	}
	if id != "" && key != "" && d.telemetry.Halted() && !d.glassChecked {
		// Glass refused the key. A person revoking it is an answer to keep;
		// a Glass that never heard of it (reinstalled) is not.
		d.glassChecked = true
		if _, err := call.Call(ctx, app, http.MethodGet, integrations.Path+"/"+id, nil); errors.Is(err, errGlassForgot) {
			id, key = "", ""
		}
	}
	if id == "" || key == "" {
		if id, key, err = d.mintGlassKey(ctx, call, app); err != nil {
			d.log.Warn("glass: connect fogd", "err", err)
			return
		}
		d.glassChecked = false
	}
	d.telemetry.Set(ep.Base, key)
}

// mintGlassKey asks Glass for fogd's own ingest key and stores it.
func (d *daemon) mintGlassKey(ctx context.Context, call *appcall.Caller, app *store.App) (id, key string, err error) {
	raw, err := call.Call(ctx, app, http.MethodPost, integrations.Path,
		map[string]string{"consumer": glassConsumer, "offer": glassOffer})
	if err != nil {
		return "", "", err
	}
	var minted struct {
		ID      string            `json:"id"`
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil || minted.ID == "" || minted.Secrets["GLASS_API_KEY"] == "" {
		return "", "", errors.New("glass answered without an integration id and ingest key")
	}
	enc, err := d.vlt.Encrypt(minted.Secrets["GLASS_API_KEY"])
	if err != nil {
		return "", "", err
	}
	if err := d.st.SetSetting(settingGlassKey, enc); err != nil {
		return "", "", err
	}
	if err := d.st.SetSetting(settingGlassIntegration, minted.ID); err != nil {
		return "", "", err
	}
	d.st.Audit("fog", "fog.glass.connect", glassConsumer, store.AuditDetail(map[string]any{"provider": glassProvider, "offer": glassOffer}))
	d.log.Info("glass: fogd now reports to Glass", "integration", minted.ID)
	return minted.ID, minted.Secrets["GLASS_API_KEY"], nil
}

// reportHost sends each host sample as fog.host.* gauges, in the units
// Glass's fog.container.* metrics use.
func (d *daemon) reportHost(s metrics.Sample) {
	c := d.telemetry
	c.Gauge("fog.host.cpu_pct", "percent", s.CPUPct, nil)
	c.Gauge("fog.host.load1", "", s.Load1, nil)
	c.Gauge("fog.host.memory_bytes", "bytes", float64(s.MemUsed), nil)
	c.Gauge("fog.host.memory_total_bytes", "bytes", float64(s.MemTotal), nil)
	c.Gauge("fog.host.disk_used_bytes", "bytes", float64(s.DiskUsed), nil)
	c.Gauge("fog.host.disk_total_bytes", "bytes", float64(s.DiskTotal), nil)
	if used, total, ok := metrics.Swap(); ok {
		c.Gauge("fog.host.swap_bytes", "bytes", float64(used), nil)
		c.Gauge("fog.host.swap_total_bytes", "bytes", float64(total), nil)
	}
}
