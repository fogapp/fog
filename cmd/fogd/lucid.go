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

// fogd reports to Lucid as the app "fog" when Lucid is installed. It asks
// for its own ingest key through the lucid/monitoring offer, as connecting
// any app on its Integrations tab would — Lucid makes the "Fog" app on its
// side — and keeps the key in the vault. Revoking that key in Lucid ›
// Settings › Apps is the off switch, and it holds: fogd mints again only
// when Lucid no longer knows the integration at all (a fresh install).
const (
	lucidProvider = "lucid"
	lucidConsumer = "fog"
	lucidOffer    = "monitoring"

	settingLucidIntegration = "lucid_integration"
	settingLucidKey         = "lucid_ingest_key" // vault-encrypted
)

// errLucidForgot: Lucid answered 404 for the integration — it is not the
// Lucid that minted it.
var errLucidForgot = errors.New("lucid does not know this integration")

func lucidRefusal(resp *http.Response, raw []byte) error {
	if resp.StatusCode == http.StatusNotFound && resp.Request.Method != http.MethodPost {
		return errLucidForgot
	}
	return integrations.Refusal(resp, raw)
}

// keepLucid is one pass of the telemetry connection: point the client at
// Lucid's address on the fog network as it is now (a redeploy moves it),
// minting a key first if fogd has none. Without a running Lucid the client
// goes inert.
func (d *daemon) keepLucid(ctx context.Context) {
	app, err := d.st.AppByName(lucidProvider)
	if err != nil || !integrations.Running(app) {
		d.telemetry.Set("", "")
		return
	}
	ep := d.engine.AppEndpoints(ctx, []store.App{*app})[0]
	if ep.Err != nil {
		d.telemetry.Set("", "")
		return
	}
	call := appcall.New(d.engine.AppEndpoints).Refusing(lucidRefusal)

	id, _ := d.st.GetSetting(settingLucidIntegration)
	key := ""
	if enc, _ := d.st.GetSetting(settingLucidKey); enc != "" {
		key, _ = d.vlt.Decrypt(enc)
	}
	if id != "" && key != "" && d.telemetry.Halted() && !d.lucidChecked {
		// Lucid refused the key. A person revoking it is an answer to keep;
		// a Lucid that never heard of it (reinstalled) is not.
		d.lucidChecked = true
		if _, err := call.Call(ctx, app, http.MethodGet, integrations.Path+"/"+id, nil); errors.Is(err, errLucidForgot) {
			id, key = "", ""
		}
	}
	if id == "" || key == "" {
		if id, key, err = d.mintLucidKey(ctx, call, app); err != nil {
			d.log.Warn("lucid: connect fogd", "err", err)
			return
		}
		d.lucidChecked = false
	}
	d.telemetry.Set(ep.Base, key)
}

// mintLucidKey asks Lucid for fogd's own ingest key and stores it.
func (d *daemon) mintLucidKey(ctx context.Context, call *appcall.Caller, app *store.App) (id, key string, err error) {
	raw, err := call.Call(ctx, app, http.MethodPost, integrations.Path,
		map[string]string{"consumer": lucidConsumer, "offer": lucidOffer})
	if err != nil {
		return "", "", err
	}
	var minted struct {
		ID      string            `json:"id"`
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil || minted.ID == "" || minted.Secrets["LUCID_API_KEY"] == "" {
		return "", "", errors.New("lucid answered without an integration id and ingest key")
	}
	enc, err := d.vlt.Encrypt(minted.Secrets["LUCID_API_KEY"])
	if err != nil {
		return "", "", err
	}
	if err := d.st.SetSetting(settingLucidKey, enc); err != nil {
		return "", "", err
	}
	if err := d.st.SetSetting(settingLucidIntegration, minted.ID); err != nil {
		return "", "", err
	}
	d.st.Audit("fog", "fog.lucid.connect", lucidConsumer, store.AuditDetail(map[string]any{"provider": lucidProvider, "offer": lucidOffer}))
	d.log.Info("lucid: fogd now reports to Lucid", "integration", minted.ID)
	return minted.ID, minted.Secrets["LUCID_API_KEY"], nil
}

// reportHost sends each host sample as fog.host.* gauges, in the units
// Lucid's fog.container.* metrics use.
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
