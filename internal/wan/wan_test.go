package wan

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublicIPFromFallsThroughSources(t *testing.T) {
	trace := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fl=123\nh=1.1.1.1\nip=203.0.113.9\nts=1\n"))
	}))
	defer trace.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer broken.Close()
	v6 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("2001:db8::1"))
	}))
	defer v6.Close()

	got, err := PublicIPFrom(context.Background(), http.DefaultClient, []Source{
		{URL: broken.URL, Parse: strings.TrimSpace},
		{URL: v6.URL, Parse: strings.TrimSpace},
		{URL: trace.URL, Parse: parseTrace},
	})
	if err != nil || got != "203.0.113.9" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := PublicIPFrom(context.Background(), http.DefaultClient, []Source{{URL: broken.URL, Parse: strings.TrimSpace}}); err == nil {
		t.Fatal("all sources failing must be an error")
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"203.0.113.9":  KindPublic,
		"192.168.1.20": KindPrivate,
		"10.0.0.1":     KindPrivate,
		"100.64.3.4":   KindCGNAT,
		"100.127.0.1":  KindCGNAT,
		"100.128.0.1":  KindPublic,
		"127.0.0.1":    KindPrivate,
		"nope":         KindInvalid,
	}
	for in, want := range cases {
		if got := Classify(in); got != want {
			t.Errorf("Classify(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestGatewayProcRoute(t *testing.T) {
	p := filepath.Join(t.TempDir(), "route")
	// 0101A8C0 is 192.168.1.1 little-endian.
	os.WriteFile(p, []byte("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n"+
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n"+
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\n"), 0o644)
	gw, err := gatewayProcRoute(p)
	if err != nil || gw.String() != "192.168.1.1" {
		t.Fatalf("got %v, %v", gw, err)
	}
}

// fakeNATPMP is a router on loopback that answers the two requests fog
// sends, and remembers which ports it mapped.
type fakeNATPMP struct {
	conn   *net.UDPConn
	mu     sync.Mutex
	mapped map[int]uint32
	// offset makes the router hand out a different external port.
	offset int
}

func startNATPMP(t *testing.T, offset int) *fakeNATPMP {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNATPMP{conn: conn, mapped: map[int]uint32{}, offset: offset}
	go func() {
		buf := make([]byte, 64)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := buf[:n]
			var resp []byte
			switch req[1] {
			case 0:
				resp = make([]byte, 12)
				resp[1] = 128
				copy(resp[8:], net.IPv4(203, 0, 113, 9).To4())
			case 2:
				in := int(binary.BigEndian.Uint16(req[4:6]))
				life := binary.BigEndian.Uint32(req[8:12])
				f.mu.Lock()
				if life == 0 {
					delete(f.mapped, in)
				} else {
					f.mapped[in] = life
				}
				f.mu.Unlock()
				resp = make([]byte, 16)
				resp[1] = 130
				binary.BigEndian.PutUint16(resp[8:10], uint16(in))
				binary.BigEndian.PutUint16(resp[10:12], uint16(in+f.offset))
				binary.BigEndian.PutUint32(resp[12:16], life)
			}
			conn.WriteToUDP(resp, addr)
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return f
}

// The real port is fixed by the protocol; tests swap it for the fake's.
func withNATPMPPort(t *testing.T, f *fakeNATPMP) {
	t.Helper()
	old := natpmpPortOverride
	natpmpPortOverride = f.conn.LocalAddr().(*net.UDPAddr).Port
	t.Cleanup(func() { natpmpPortOverride = old })
}

func TestMapperNATPMP(t *testing.T) {
	f := startNATPMP(t, 0)
	withNATPMPPort(t, f)
	m := &Mapper{Gateway: func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil }, NoUPnP: true}
	mp, err := m.Map(context.Background(), "192.168.1.200", []int{443, 80}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Method != "natpmp" || mp.ExternalIP != "203.0.113.9" || len(mp.Opened) != 2 || mp.Opened[0] != 80 {
		t.Fatalf("unexpected mapping: %+v", mp)
	}
	f.mu.Lock()
	life := f.mapped[443]
	f.mu.Unlock()
	if life != 3600 {
		t.Fatalf("router recorded lifetime %d", life)
	}
	if err := m.Unmap(context.Background(), "natpmp", []int{443, 80}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	left := len(f.mapped)
	f.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d mappings still held after Unmap", left)
	}
}

// A router that hands out a different external port has not given us
// 443 — the hostname would answer nowhere. That is a failure, and the
// stray mapping must be released rather than left behind.
func TestMapperNATPMPRejectsRemappedPort(t *testing.T) {
	f := startNATPMP(t, 1000)
	withNATPMPPort(t, f)
	m := &Mapper{Gateway: func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil }, NoUPnP: true}
	if _, err := m.Map(context.Background(), "192.168.1.200", []int{443}, time.Hour); err == nil {
		t.Fatal("expected failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mapped) != 0 {
		t.Fatalf("stray mapping left behind: %v", f.mapped)
	}
}

// fakeIGD serves a UPnP gateway description and a control endpoint. The
// first AddPortMapping with a lease is refused with 725 so the test proves
// the permanent-lease retry.
func startIGD(t *testing.T, permanentOnly bool) (*httptest.Server, *[]string) {
	return startIGDWith(t, permanentOnly, "")
}

// startIGDWith can also pre-load a conflicting mapping: AddPortMapping is
// refused with 718 until a DeletePortMapping clears it.
func startIGDWith(t *testing.T, permanentOnly bool, conflictClient string) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/desc.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><root><device><deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
<deviceList><device><deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType><deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType><serviceList><service>
<serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>/ctl</controlURL>
</service></serviceList></device></deviceList></device></deviceList></device></root>`))
	})
	mux.HandleFunc("/ctl", func(w http.ResponseWriter, r *http.Request) {
		action := strings.Trim(strings.SplitN(r.Header.Get("SOAPAction"), "#", 2)[1], `"`)
		body := make([]byte, 8192)
		n, _ := r.Body.Read(body)
		s := string(body[:n])
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, action)
		switch action {
		case "GetExternalIPAddress":
			w.Write([]byte(`<s:Envelope><s:Body><u:GetExternalIPAddressResponse><NewExternalIPAddress>100.70.1.2</NewExternalIPAddress></u:GetExternalIPAddressResponse></s:Body></s:Envelope>`))
		case "AddPortMapping":
			if conflictClient != "" && !strings.Contains(s, "<NewInternalClient>"+conflictClient+"</NewInternalClient>") {
				w.WriteHeader(500)
				w.Write([]byte(`<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>718</errorCode><errorDescription>ConflictInMappingEntry</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`))
				return
			}
			if permanentOnly && !strings.Contains(s, "<NewLeaseDuration>0</NewLeaseDuration>") {
				w.WriteHeader(500)
				w.Write([]byte(`<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>725</errorCode><errorDescription>OnlyPermanentLeasesSupported</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`))
				return
			}
			if !strings.Contains(s, "<NewInternalClient>192.168.1.200</NewInternalClient>") {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(`<s:Envelope><s:Body><u:AddPortMappingResponse/></s:Body></s:Envelope>`))
		case "DeletePortMapping":
			conflictClient = ""
			w.Write([]byte(`<s:Envelope><s:Body><u:DeletePortMappingResponse/></s:Body></s:Envelope>`))
		default:
			w.WriteHeader(500)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestMapperUPnPFallsBackToPermanentLease(t *testing.T) {
	srv, calls := startIGD(t, true)
	m := &Mapper{
		Gateway:   func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil },
		Locations: []string{srv.URL + "/desc.xml"},
		NoNATPMP:  true,
	}
	mp, err := m.Map(context.Background(), "192.168.1.200", []int{443}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Method != "upnp" || !mp.Permanent || mp.ExternalIP != "100.70.1.2" || len(mp.Opened) != 1 {
		t.Fatalf("unexpected mapping: %+v", mp)
	}
	if Classify(mp.ExternalIP) != KindCGNAT {
		t.Fatal("router WAN address in 100.64/10 must classify as CGNAT")
	}
	adds := 0
	for _, c := range *calls {
		if c == "AddPortMapping" {
			adds++
		}
	}
	if adds != 2 {
		t.Fatalf("expected the lease retry (2 AddPortMapping calls), got %d", adds)
	}
	if err := m.Unmap(context.Background(), "upnp", []int{443}); err != nil {
		t.Fatal(err)
	}
}

func TestMapperReportsBothProtocolsWhenNeitherAnswers(t *testing.T) {
	f := startNATPMP(t, 0)
	f.conn.Close() // nothing listening
	withNATPMPPort(t, f)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer dead.Close()
	m := &Mapper{
		Gateway:   func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil },
		Locations: []string{dead.URL + "/desc.xml"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.Map(ctx, "192.168.1.200", []int{443}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "NAT-PMP:") || !strings.Contains(err.Error(), "UPnP:") {
		t.Fatalf("error should name both protocols: %v", err)
	}
}

// After a DHCP renumbering the router still holds 443 → the old address
// and refuses a new mapping with 718. That mapping is ours: delete it
// and retry rather than report a conflict the operator can't see.
func TestMapperUPnPReplacesConflictingMapping(t *testing.T) {
	srv, calls := startIGDWith(t, false, "192.168.1.77")
	m := &Mapper{
		Gateway:   func() (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil },
		Locations: []string{srv.URL + "/desc.xml"},
		NoNATPMP:  true,
	}
	mp, err := m.Map(context.Background(), "192.168.1.200", []int{443}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(mp.Opened) != 1 {
		t.Fatalf("mapping: %+v", mp)
	}
	seq := strings.Join(*calls, ",")
	if !strings.Contains(seq, "AddPortMapping,DeletePortMapping,AddPortMapping") {
		t.Fatalf("expected add → delete → add, got %s", seq)
	}
}
