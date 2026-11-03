package dockerx

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
)

func summary(id, process, aux, state string) ContainerSummary {
	labels := map[string]string{}
	if process != "" {
		labels["fog.process"] = process
	}
	if aux != "" {
		labels["fog.aux"] = aux
	}
	return ContainerSummary{ID: id, State: state, Labels: labels}
}

// An app with a pg or worker sidecar must resolve to its main container no
// matter what order Docker lists them in — logs and exec attached to
// containers[0] used to show the sidecar on list-order luck.
func TestContainerForProcess(t *testing.T) {
	main := summary("main", "", "", "running")
	worker := summary("worker", "worker", "", "running")
	pg := summary("pg", "", "pg", "running")

	cases := []struct {
		name       string
		containers []ContainerSummary
		process    string
		wantID     string
		wantOK     bool
	}{
		{"main wins regardless of order", []ContainerSummary{pg, worker, main}, "", "main", true},
		{"named process selected", []ContainerSummary{main, worker}, "worker", "worker", true},
		{"unknown process", []ContainerSummary{main, worker}, "web2", "", false},
		{"aux sidecar is never the main", []ContainerSummary{pg, worker}, "", "", false},
		{"no containers", nil, "", "", false},
		{
			// Mid-swap the old main may linger exited beside its replacement;
			// the running one is the app.
			"running main preferred over exited",
			[]ContainerSummary{summary("old", "", "", "exited"), main}, "", "main", true,
		},
		{
			// A crashed app still has logs worth reading: an exited main is
			// returned rather than nothing.
			"exited main still found",
			[]ContainerSummary{pg, summary("crashed", "", "", "exited")}, "", "crashed", true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := ContainerForProcess(tc.containers, tc.process)
			if ok != tc.wantOK || (ok && c.ID != tc.wantID) {
				t.Errorf("ContainerForProcess(%q) = %q, %v; want %q, %v", tc.process, c.ID, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// A stale exited replica listed before (or after) the running main must not
// shadow it, whatever Docker's order; an app with only an exited main still
// maps to it.
func TestMainContainers(t *testing.T) {
	app := func(c ContainerSummary, name string) ContainerSummary {
		c.Labels["fog.app"] = name
		return c
	}
	cs := []ContainerSummary{
		app(summary("stale", "", "", "exited"), "web"),
		app(summary("live", "", "", "running"), "web"),
		app(summary("stale2", "", "", "exited"), "web"),
		app(summary("worker", "worker", "", "running"), "web"),
		app(summary("crashed", "", "", "exited"), "blog"),
		app(summary("pg", "", "pg", "running"), "blog"),
	}
	got := MainContainers(cs)
	if got["web"].ID != "live" {
		t.Errorf("web main = %q, want live", got["web"].ID)
	}
	if got["blog"].ID != "crashed" {
		t.Errorf("blog main = %q, want crashed", got["blog"].ID)
	}
	if len(got) != 2 {
		t.Errorf("got %d apps, want 2", len(got))
	}
}

// frame encodes one Docker stream-multiplexing frame around the payload.
func frame(payload string) []byte {
	hdr := make([]byte, 8)
	hdr[0] = 1 // stdout
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	return append(hdr, payload...)
}

// errAfterReader yields its buffer, then a non-EOF error — a socket dying
// mid-stream.
type errAfterReader struct {
	io.Reader
	err error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return n, r.err
	}
	return n, err
}

func (r *errAfterReader) Close() error { return nil }

func TestDemuxReaderCleanEnd(t *testing.T) {
	stream := append(frame("hello "), frame("world")...)
	b, err := io.ReadAll(demuxReader(io.NopCloser(bytes.NewReader(stream))))
	if err != nil {
		t.Fatalf("clean stream: %v", err)
	}
	if string(b) != "hello world" {
		t.Errorf("payload = %q, want %q", b, "hello world")
	}
}

// A follower whose stream dies mid-frame must see an error, not a quiet
// end-of-logs that reads as "the container went silent".
func TestDemuxReaderPropagatesStreamError(t *testing.T) {
	socketErr := errors.New("connection reset")
	rc := &errAfterReader{Reader: bytes.NewReader(frame("partial")), err: socketErr}
	b, err := io.ReadAll(demuxReader(rc))
	if string(b) != "partial" {
		t.Errorf("payload before error = %q, want %q", b, "partial")
	}
	if !errors.Is(err, socketErr) {
		t.Errorf("err = %v, want the underlying %v", err, socketErr)
	}
}

func TestDemuxReaderTruncatedPayload(t *testing.T) {
	full := frame("truncated")
	rc := io.NopCloser(bytes.NewReader(full[:len(full)-3]))
	if _, err := io.ReadAll(demuxReader(rc)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated payload: err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestDemuxReaderTruncatedHeader(t *testing.T) {
	stream := append(frame("ok"), 1, 0, 0)
	if _, err := io.ReadAll(demuxReader(io.NopCloser(bytes.NewReader(stream)))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated header: err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// sealedEngine serves one network's inspect, creating it on POST.
type sealedEngine struct {
	mu      sync.Mutex
	exists  string // the inspect body once the network exists; "" before
	created string // the create request's body
	make    string // what a create leaves the network as
}

func (f *sealedEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		f.created, f.exists = string(body), f.make
		w.WriteHeader(http.StatusCreated)
		return
	}
	if f.exists == "" {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"no such network"}`)
		return
	}
	io.WriteString(w, f.exists)
}

const internalNetwork = `{"Internal":true,"IPAM":{"Config":[{"Subnet":"fd00::/64","Gateway":"fd00::1"},{"Subnet":"172.30.0.0/16","Gateway":"172.30.0.1"}]}}`

func TestEnsureInternalNetworkCreatesIt(t *testing.T) {
	f := &sealedEngine{make: internalNetwork}
	gw, err := engineClient(t, f).EnsureInternalNetwork(context.Background(), "fog-sealed")
	if err != nil {
		t.Fatal(err)
	}
	if gw != "172.30.0.1" {
		t.Errorf("gateway = %q", gw)
	}
	var body struct {
		Name, Driver string
		Internal     bool
	}
	if err := json.Unmarshal([]byte(f.created), &body); err != nil {
		t.Fatal(err)
	}
	if body.Name != "fog-sealed" || body.Driver != "bridge" || !body.Internal {
		t.Errorf("created %s", f.created)
	}
}

func TestEnsureInternalNetworkKeepsWhatExists(t *testing.T) {
	f := &sealedEngine{exists: internalNetwork}
	gw, err := engineClient(t, f).EnsureInternalNetwork(context.Background(), "fog-sealed")
	if err != nil || gw != "172.30.0.1" {
		t.Fatalf("gateway = %q, %v", gw, err)
	}
	if f.created != "" {
		t.Errorf("recreated it: %s", f.created)
	}
}

// A network of the right name with a way out would make every sealed app a
// lie, so it is refused rather than joined.
func TestEnsureInternalNetworkRefusesARouteOut(t *testing.T) {
	f := &sealedEngine{exists: `{"Internal":false,"IPAM":{"Config":[{"Gateway":"172.30.0.1"}]}}`}
	if _, err := engineClient(t, f).EnsureInternalNetwork(context.Background(), "fog-sealed"); err == nil {
		t.Fatal("joined a network with a route out")
	}
}

func TestEnsureInternalNetworkNeedsAGateway(t *testing.T) {
	f := &sealedEngine{exists: `{"Internal":true,"IPAM":{"Config":[]}}`}
	if _, err := engineClient(t, f).EnsureInternalNetwork(context.Background(), "fog-sealed"); err == nil {
		t.Fatal("answered no gateway")
	}
}

func TestNetworkInternal(t *testing.T) {
	c := engineClient(t, &sealedEngine{exists: `{"Internal":false}`})
	if internal, err := c.NetworkInternal(context.Background(), "fog"); err != nil || internal {
		t.Errorf("internal = %v, %v", internal, err)
	}
	c = engineClient(t, &sealedEngine{})
	if _, err := c.NetworkInternal(context.Background(), "gone"); err == nil {
		t.Error("a missing network read as an answer")
	}
}
