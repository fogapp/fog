package dockerx

import (
	"encoding/json"
	"testing"
)

// Docker 27 inspects a network created with a subnet and no gateway with
// the subnet alone; its default IPAM gave the bridge the first host address.
func TestNetworkGatewayFallsBackToTheSubnetsFirstHost(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
	}{
		{"reported", `[{"Subnet":"10.231.7.0/24","Gateway":"10.231.7.1"}]`, "10.231.7.1"},
		{"reported elsewhere", `[{"Subnet":"10.231.7.0/24","Gateway":"10.231.7.254"}]`, "10.231.7.254"},
		{"unreported", `[{"Subnet":"10.231.7.0/24"}]`, "10.231.7.1"},
		{"unmasked subnet", `[{"Subnet":"10.231.7.9/24"}]`, "10.231.7.1"},
		{"ipv6 first", `[{"Subnet":"fd00::/64"},{"Subnet":"172.30.4.0/22"}]`, "172.30.4.1"},
		{"ipv6 only", `[{"Subnet":"fd00::/64"}]`, ""},
		{"too small", `[{"Subnet":"10.0.0.0/31"}]`, ""},
		{"none", `[]`, ""},
	} {
		var n networkDetail
		if err := json.Unmarshal([]byte(`{"IPAM":{"Config":`+tc.config+`}}`), &n); err != nil {
			t.Fatal(err)
		}
		if got := n.gateway(); got != tc.want {
			t.Errorf("%s: gateway() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
