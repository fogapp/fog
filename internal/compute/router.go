package compute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"gitea.lan/matthew/fog/internal/store"
)

// RouterPath is where a router app answers fogd's question.
const RouterPath = "/fog/inference/route"

// errNoRouter is a household with no app routing inference.
var errNoRouter = errors.New("no app routes inference")

// NewRouter asks the app that declared fog-inference-router/1 which
// machine should answer each request (think-fleet, which knows which Mac
// has the model warm). With none installed, or none answering, fogd orders
// the machines itself.
func NewRouter(st *store.Store, apps Caller) func(ctx context.Context, ask RouteAsk) ([]string, error) {
	return func(ctx context.Context, ask RouteAsk) ([]string, error) {
		routers, err := st.AppsWithContract(RouterContract)
		if err != nil || len(routers) == 0 {
			return nil, errors.Join(errNoRouter, err)
		}
		raw, err := apps.Call(ctx, &routers[0], http.MethodPost, RouterPath, ask)
		if err != nil {
			return nil, err
		}
		var got struct {
			Machines []string `json:"machines"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			return nil, err
		}
		return got.Machines, nil
	}
}
