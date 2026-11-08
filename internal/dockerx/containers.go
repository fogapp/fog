package dockerx

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type ContainerSummary struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
	// ImageID is the exact image the container runs, whatever its tag
	// has been moved to since.
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`  // running, exited, …
	Status  string            `json:"Status"` // "Up 2 hours"
	Labels  map[string]string `json:"Labels"`
	// NetworkSettings is where the container sits on each network. The
	// list call carries it, so an address needs no inspect per container.
	NetworkSettings SummaryNetworks `json:"NetworkSettings"`
	// Ports are its ports, with the host port each is published on.
	Ports []ContainerPort `json:"Ports"`
}

// ContainerPort is one of a listed container's ports. PublicPort is 0 when
// it isn't published on the host.
type ContainerPort struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

// SummaryNetworks is a listed container's addresses, by network name.
type SummaryNetworks struct {
	Networks map[string]NetworkEndpoint `json:"Networks"`
}

// NetworkEndpoint is a container's place on one network.
type NetworkEndpoint struct {
	IPAddress string `json:"IPAddress"`
	// Aliases are the names asked for on this network; DNSNames (API 1.44+)
	// every name the container answers to there.
	Aliases  []string `json:"Aliases"`
	DNSNames []string `json:"DNSNames"`
}

// Answers reports whether the endpoint answers to name.
func (e NetworkEndpoint) Answers(name string) bool {
	for _, n := range e.Aliases {
		if n == name {
			return true
		}
	}
	for _, n := range e.DNSNames {
		if n == name {
			return true
		}
	}
	return false
}

// FogSystem names which of fog's own containers c is — "caddy", "wire" or
// "postgres" — or "" for any other. By the container name fog gives each,
// never by the fog.system label: an image's LABEL lands on its container,
// and an app whose image said fog.system=caddy was put on every app's
// network as the proxy. No app container can have one of these names:
// compose names an app's containers <project>-<service>-<n>.
func FogSystem(c ContainerSummary) string {
	for _, n := range c.Names {
		switch n {
		case "/caddy":
			return "caddy"
		case "/fog-wire":
			return "wire"
		case "/fog-postgres":
			return "postgres"
		}
	}
	return ""
}

// IsMainContainer reports whether a container is an app's main (routed)
// process, as opposed to one of its worker processes or an aux sidecar.
//
// Shared rather than re-derived per caller: the definition has to agree
// everywhere, and versions that checked only fog.process quietly treated a
// sidecar as the app — attributing its image, its port, and its health to
// the app itself.
func IsMainContainer(c ContainerSummary) bool {
	return c.Labels["fog.process"] == "" && c.Labels["fog.aux"] == ""
}

// ContainerForProcess picks the container an operator means when they name an
// app: the main container when process is "", or the container whose
// fog.process label matches. Shared by exec and both log surfaces so that an
// app with a pg or worker sidecar never shows the wrong container's logs on
// Docker list-order luck. When more than one candidate matches (an exited
// container beside its replacement mid-swap), a running one wins; a stopped
// match is still returned so crash logs stay reachable.
func ContainerForProcess(containers []ContainerSummary, process string) (ContainerSummary, bool) {
	var found ContainerSummary
	ok := false
	for _, c := range containers {
		if (process == "" && !IsMainContainer(c)) || (process != "" && c.Labels["fog.process"] != process) {
			continue
		}
		if c.State == "running" {
			return c, true
		}
		if !ok {
			found, ok = c, true
		}
	}
	return found, ok
}

// ErrNoContainer: the app has no containers at all. ErrNoProcess: it has
// some, but none for the process asked for.
var (
	ErrNoContainer = errors.New("no container for app")
	ErrNoProcess   = errors.New("no such process for app")
)

// ProcessContainer lists app's containers and picks process's (the main
// container when process is empty) by ContainerForProcess's rule. The
// container may be stopped; callers that need it running check State.
func (c *Client) ProcessContainer(ctx context.Context, app, process string) (ContainerSummary, error) {
	cs, err := c.ListByLabel(ctx, "fog.app="+app)
	if err != nil {
		return ContainerSummary{}, err
	}
	if len(cs) == 0 {
		return ContainerSummary{}, ErrNoContainer
	}
	found, ok := ContainerForProcess(cs, process)
	if !ok {
		return ContainerSummary{}, ErrNoProcess
	}
	return found, nil
}

// MainContainers maps each app (its fog.app label) to its main container,
// by ContainerForProcess's rule: a running one wins, else the first listed
// (Docker lists newest first). Keeping the last match instead let an exited
// replica left by a failed drain shadow the running container, so the
// dashboard said "exited" and the scanner filed drift for a healthy app.
func MainContainers(containers []ContainerSummary) map[string]ContainerSummary {
	out := map[string]ContainerSummary{}
	for _, c := range containers {
		if !IsMainContainer(c) {
			continue
		}
		app := c.Labels["fog.app"]
		if cur, ok := out[app]; ok && (cur.State == "running" || c.State != "running") {
			continue
		}
		out[app] = c
	}
	return out
}

// ListByLabel returns all containers (running or not) carrying the label.
func (c *Client) ListByLabel(ctx context.Context, label string) ([]ContainerSummary, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []ContainerSummary
	err := c.get(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), &out)
	return out, err
}

type ContainerDetail struct {
	ID string `json:"Id"`
	// Image is the resolved image ID (sha256:…); Config.Image is the
	// reference the container was created from (repo:tag).
	Image string `json:"Image"`
	// RestartCount is Docker's own tally of restarts under the restart
	// policy since the container was created.
	RestartCount int `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"` // created, running, exited, …
		Running    bool   `json:"Running"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		Restarting bool   `json:"Restarting"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
		// Env is the container's baked-in environment ("KEY=value"), which
		// is what the app actually runs with — the spec says what the next
		// deploy would give it, which is not the same thing.
		Env []string `json:"Env"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string   `json:"IPAddress"`
			Aliases   []string `json:"Aliases"`
			DNSNames  []string `json:"DNSNames"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (c *Client) Inspect(ctx context.Context, id string) (*ContainerDetail, error) {
	d := &ContainerDetail{}
	if err := c.get(ctx, "/containers/"+id+"/json", d); err != nil {
		return nil, err
	}
	return d, nil
}

// Logs streams container logs. The raw stream is multiplexed with 8-byte
// frame headers (stdout/stderr); demux() strips them. With timestamps each
// line is prefixed by Docker's RFC3339Nano stamp and a space, which is what
// lets a reader interleave fogd's own actions with the app's output.
func (c *Client) Logs(ctx context.Context, id string, follow bool, tail string, timestamps bool) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	if timestamps {
		q.Set("timestamps", "1")
	} else {
		q.Set("timestamps", "0")
	}
	if follow {
		q.Set("follow", "1")
	}
	if tail != "" {
		q.Set("tail", tail)
	}
	// c.stream, not c.http: a follow stream must not be cut off by the
	// client-wide timeout.
	resp, err := c.send(ctx, c.stream, http.MethodGet, "/containers/"+id+"/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return demuxReader(resp.Body), nil
}

// demuxReader strips Docker's stream multiplexing headers:
// [stream(1)][pad(3)][size(4, BE)][payload…]
func demuxReader(rc io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer rc.Close()
		hdr := make([]byte, 8)
		for {
			if _, err := io.ReadFull(rc, hdr); err != nil {
				// A clean stream ends exactly on a frame boundary (io.EOF).
				// Anything else — a truncated header, a socket error — must
				// reach the reader as an error, or a log follower silently
				// ends as if the logs simply finished.
				if err == io.EOF {
					err = nil
				}
				pw.CloseWithError(err)
				return
			}
			size := binary.BigEndian.Uint32(hdr[4:8])
			if size > 0 {
				if _, err := io.CopyN(pw, rc, int64(size)); err != nil {
					// EOF here means the payload was cut short — that is
					// truncation, not a finished stream.
					if err == io.EOF {
						err = io.ErrUnexpectedEOF
					}
					pw.CloseWithError(err)
					return
				}
			}
		}
	}()
	return struct {
		io.Reader
		io.Closer
	}{pr, closerFunc(func() error { pr.Close(); return rc.Close() })}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

type Stats struct {
	CPUPercent float64 `json:"cpuPercent"`
	MemUsage   uint64  `json:"memUsage"`
	MemLimit   uint64  `json:"memLimit"`
	// Cumulative counters since the container started, summed over every
	// interface and block device. Rates are the sampler's job: a single
	// reading cannot say how fast anything is moving.
	NetRx    uint64 `json:"netRx"`
	NetTx    uint64 `json:"netTx"`
	BlkRead  uint64 `json:"blkRead"`
	BlkWrite uint64 `json:"blkWrite"`
	// Raw cumulative CPU counters (nanoseconds) and the CPU count, for a
	// sampler that computes CPUPercent between its own readings.
	CPUTotal   uint64 `json:"-"`
	SystemCPU  uint64 `json:"-"`
	OnlineCPUs int    `json:"-"`
}

// StatsOnce takes a single stats sample with CPUPercent filled in. Docker
// computes the CPU delta between two internal samples a second apart, so the
// call takes about a second: fine for a one-off reading, too slow for a
// sampler visiting every container (that's StatsSnapshot).
func (c *Client) StatsOnce(ctx context.Context, id string) (*Stats, error) {
	return c.stats(ctx, id, false)
}

// StatsSnapshot reads a container's counters without Docker's one-second
// CPU sampling wait (one-shot). CPUPercent stays zero; the caller turns
// CPUTotal/SystemCPU into a rate between its own successive snapshots.
func (c *Client) StatsSnapshot(ctx context.Context, id string) (*Stats, error) {
	return c.stats(ctx, id, true)
}

func (c *Client) stats(ctx context.Context, id string, oneShot bool) (*Stats, error) {
	var raw struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs  int    `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Limit uint64 `json:"limit"`
			Stats struct {
				InactiveFile uint64 `json:"inactive_file"`
			} `json:"stats"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			RxBytes uint64 `json:"rx_bytes"`
			TxBytes uint64 `json:"tx_bytes"`
		} `json:"networks"`
		BlkioStats struct {
			IOServiceBytes []struct {
				Op    string `json:"op"`
				Value uint64 `json:"value"`
			} `json:"io_service_bytes_recursive"`
		} `json:"blkio_stats"`
	}
	path := "/containers/" + id + "/stats?stream=false"
	if oneShot {
		path += "&one-shot=true"
	}
	if err := c.get(ctx, path, &raw); err != nil {
		return nil, err
	}
	st := &Stats{
		MemLimit:   raw.MemoryStats.Limit,
		CPUTotal:   raw.CPUStats.CPUUsage.TotalUsage,
		SystemCPU:  raw.CPUStats.SystemUsage,
		OnlineCPUs: raw.CPUStats.OnlineCPUs,
	}
	for _, n := range raw.Networks {
		st.NetRx += n.RxBytes
		st.NetTx += n.TxBytes
	}
	for _, b := range raw.BlkioStats.IOServiceBytes {
		switch strings.ToLower(b.Op) {
		case "read":
			st.BlkRead += b.Value
		case "write":
			st.BlkWrite += b.Value
		}
	}
	// Subtract page cache the way `docker stats` does.
	if raw.MemoryStats.Usage > raw.MemoryStats.Stats.InactiveFile {
		st.MemUsage = raw.MemoryStats.Usage - raw.MemoryStats.Stats.InactiveFile
	}
	if !oneShot {
		st.CPUPercent = CPUPercent(raw.PreCPUStats.CPUUsage.TotalUsage, raw.PreCPUStats.SystemUsage,
			st.CPUTotal, st.SystemCPU, st.OnlineCPUs)
	}
	return st, nil
}

// CPUPercent is `docker stats`' CPU figure between two readings of a
// container's cumulative CPU time and the host's: 100 per fully busy core.
// Zero when either counter didn't advance (no earlier reading, or a restart
// that reset them).
func CPUPercent(prevTotal, prevSystem, total, system uint64, onlineCPUs int) float64 {
	if total <= prevTotal || system <= prevSystem {
		return 0
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}
	return float64(total-prevTotal) / float64(system-prevSystem) * float64(onlineCPUs) * 100
}

// EnsureNetwork creates the shared bridge network if it doesn't exist. Only
// the daemon saying "no such network" counts as missing: a timeout or a 500
// while asking says nothing about the network, and is an error rather than
// a reason to try creating one.
func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	err := c.get(ctx, "/networks/"+url.PathEscape(name), nil)
	if err == nil {
		return nil
	}
	if !hasStatus(err, http.StatusNotFound) {
		return fmt.Errorf("checking network %s: %w", name, err)
	}
	err = c.do(ctx, c.http, http.MethodPost, "/networks/create", map[string]any{"Name": name, "Driver": "bridge"}, nil)
	if hasStatus(err, http.StatusConflict) { // created by someone else since we asked
		return nil
	}
	return err
}

// networkDetail is the part of a network's inspect fog reads.
type networkDetail struct {
	Internal bool `json:"Internal"`
	IPAM     struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// gateway is the network's IPv4 gateway, or "". Docker 27 doesn't always
// report one it chose itself: a network created with a subnet and no
// gateway (as fog creates its isolated networks) inspects with the subnet
// alone. Docker's default IPAM then gives the bridge the subnet's first
// host address, so that is the gateway when none is reported.
func (n *networkDetail) gateway() string {
	for _, c := range n.IPAM.Config {
		if ip, err := netip.ParseAddr(c.Gateway); err == nil && ip.Is4() {
			return c.Gateway
		}
	}
	for _, c := range n.IPAM.Config {
		if c.Gateway != "" {
			continue
		}
		p, err := netip.ParsePrefix(c.Subnet)
		if err != nil || !p.Addr().Is4() || p.Bits() > 30 {
			continue
		}
		return p.Masked().Addr().Next().String()
	}
	return ""
}

// NetworkInternal reports whether Docker gives a network no route out.
func (c *Client) NetworkInternal(ctx context.Context, name string) (bool, error) {
	var n networkDetail
	if err := c.get(ctx, "/networks/"+url.PathEscape(name), &n); err != nil {
		return false, err
	}
	return n.Internal, nil
}

// EnsureInternalNetwork creates a bridge network with no route out if it
// doesn't exist, and answers its gateway: the host's own address on it, on
// the containers' subnet. A network of that name that does have a route out
// is refused, not reused. Everything sealed about the apps on it rests on
// that flag, and Docker can't change it on a network that exists.
func (c *Client) EnsureInternalNetwork(ctx context.Context, name string) (gateway string, err error) {
	var n networkDetail
	err = c.get(ctx, "/networks/"+url.PathEscape(name), &n)
	if hasStatus(err, http.StatusNotFound) {
		err = c.do(ctx, c.http, http.MethodPost, "/networks/create", map[string]any{"Name": name, "Driver": "bridge", "Internal": true}, nil)
		if err != nil && !hasStatus(err, http.StatusConflict) { // conflict: created by someone else since we asked
			return "", err
		}
		err = c.get(ctx, "/networks/"+url.PathEscape(name), &n)
	}
	if err != nil {
		return "", fmt.Errorf("checking network %s: %w", name, err)
	}
	if !n.Internal {
		return "", fmt.Errorf("network %s exists with a route out; remove it and fog will recreate it as internal", name)
	}
	if gateway = n.gateway(); gateway == "" {
		return "", fmt.Errorf("network %s has no IPv4 gateway for its containers to reach this host on", name)
	}
	return gateway, nil
}

// probeTimeout for container health probes.
const probeTimeout = 5 * time.Second

// ProbeClient is the client for health probes against a container. It never
// follows a redirect: an app that answers its health path with a redirect to
// https://<its fqdn> would otherwise send the probe out through Caddy and
// DNS, and the verdict would describe the proxy rather than the container.
// A 3xx is an answer, and under 500 it counts as healthy.
var ProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// ProbeHTTP checks an HTTP endpoint from the host (works on Linux where
// container bridge IPs are host-routable; returns false on macOS dev).
func ProbeHTTP(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", addr, nil)
	if err != nil {
		return false
	}
	resp, err := ProbeClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}
