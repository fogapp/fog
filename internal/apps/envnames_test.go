package apps

import (
	"context"
	"testing"
)

// A provider's declared offer may not name fog's own secrets, a variable
// fog sets, or one that changes what the consumer runs; nor may a want
// rename onto one, nor any save set a "_" secret.
func TestOfferedNamesAreChecked(t *testing.T) {
	for _, n := range []string{"_fog_api_token", "_webhook_token", "_deploy_key", "FOG_API_TOKEN", "FLARE_DSN", "OIDC_CLIENT_SECRET",
		"LD_PRELOAD", "NODE_OPTIONS", "PYTHONPATH", "RUBYOPT", "HTTP_PROXY", "https_proxy", "NO_PROXY", "PATH", "BASH_ENV"} {
		if err := validateOffers([]Offer{{ID: "o", Title: "O", Secrets: []string{n}}}); err == nil {
			t.Errorf("offer secret %s accepted", n)
		}
		if err := validateOffers([]Offer{{ID: "o", Title: "O", Env: map[string]string{n: "x"}}}); err == nil {
			t.Errorf("offer env %s accepted", n)
		}
		if err := validateWants([]Want{{Offer: "p/o", As: map[string]string{"KEY": n}}}); err == nil {
			t.Errorf("rename onto %s accepted", n)
		}
	}
	if err := validateOffers([]Offer{{ID: "o", Title: "O", Env: map[string]string{"GLASS_URL": "{{url}}"}, Secrets: []string{"GLASS_API_KEY"}}}); err != nil {
		t.Errorf("an ordinary offer refused: %v", err)
	}
	e, _ := routeEngine(t, "")
	if err := e.Create(context.Background(), "a", routed(), map[string]string{"_fog_api_token": "x"}, "t"); err == nil {
		t.Error("created an app with a _ secret")
	}
	if err := e.Create(context.Background(), "b", routed(), nil, "t"); err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateSpec(context.Background(), "b", routed(), map[string]string{"_deploy_key": "x"}, "", "t"); err == nil {
		t.Error("saved a _ secret")
	}
}
