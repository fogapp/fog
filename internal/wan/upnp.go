package wan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// UPnP IGD: the older, chattier protocol. Discovery is an SSDP multicast
// search; the router answers with the URL of an XML description; inside it
// is a WANIPConnection (or WANPPPConnection) service whose control URL
// takes SOAP calls. fog needs three of those: GetExternalIPAddress,
// AddPortMapping and DeletePortMapping.

const ssdpAddr = "239.255.255.250:1900"

var igdSearchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
}

var wanServiceTypes = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// upnpDiscover multicasts M-SEARCH and collects description URLs for the
// window given. Most routers answer within a second; the window is capped
// so a LAN with no IGD costs the caller no more than that.
func upnpDiscover(ctx context.Context, window time.Duration) ([]string, error) {
	dst, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	for _, st := range igdSearchTargets {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: " + ssdpAddr + "\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(msg), dst); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(window)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetReadDeadline(deadline)
	seen := map[string]bool{}
	var out []string
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc != "" && !seen[loc] {
			seen[loc] = true
			out = append(out, loc)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no UPnP gateway answered")
	}
	return out, nil
}

type upnpDesc struct {
	URLBase string     `xml:"URLBase"`
	Device  upnpDevice `xml:"device"`
}

type upnpDevice struct {
	DeviceType string        `xml:"deviceType"`
	Services   []upnpService `xml:"serviceList>service"`
	Devices    []upnpDevice  `xml:"deviceList>device"`
}

type upnpService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

// igd is one usable WAN connection service on a gateway.
type igd struct {
	ServiceType string
	ControlURL  string
	client      *http.Client
}

// upnpResolve fetches a description and finds its WAN connection service.
func upnpResolve(ctx context.Context, client *http.Client, location string) (*igd, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var desc upnpDesc
	if err := xml.Unmarshal(body, &desc); err != nil {
		return nil, fmt.Errorf("device description: %w", err)
	}
	base, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	if desc.URLBase != "" {
		if b, err := url.Parse(desc.URLBase); err == nil {
			base = b
		}
	}
	for _, want := range wanServiceTypes {
		if svc := findService(&desc.Device, want); svc != nil {
			ctl, err := base.Parse(svc.ControlURL)
			if err != nil {
				return nil, err
			}
			return &igd{ServiceType: svc.ServiceType, ControlURL: ctl.String(), client: client}, nil
		}
	}
	return nil, errors.New("gateway description has no WAN connection service")
}

func findService(d *upnpDevice, typ string) *upnpService {
	for i := range d.Services {
		if d.Services[i].ServiceType == typ {
			return &d.Services[i]
		}
	}
	for i := range d.Devices {
		if s := findService(&d.Devices[i], typ); s != nil {
			return s
		}
	}
	return nil
}

var (
	soapErrCode = regexp.MustCompile(`<errorCode>(\d+)</errorCode>`)
	soapErrDesc = regexp.MustCompile(`<errorDescription>([^<]*)</errorDescription>`)
)

// upnpError is a SOAP fault; Code is the UPnP error code (725 =
// OnlyPermanentLeasesSupported, 718 = ConflictInMappingEntry, ...).
type upnpError struct {
	Code int
	Desc string
}

func (e *upnpError) Error() string {
	if e.Desc != "" {
		return fmt.Sprintf("%s (UPnP error %d)", e.Desc, e.Code)
	}
	return fmt.Sprintf("UPnP error %d", e.Code)
}

// call issues one SOAP action; args are emitted in order. Returns the raw
// response body for the caller to pick values out of.
func (g *igd) call(ctx context.Context, action string, args [][2]string) (string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:`)
	b.WriteString(action)
	b.WriteString(` xmlns:u="` + g.ServiceType + `">`)
	for _, a := range args {
		b.WriteString("<" + a[0] + ">")
		xml.EscapeText(&b, []byte(a[1]))
		b.WriteString("</" + a[0] + ">")
	}
	b.WriteString("</u:" + action + "></s:Body></s:Envelope>")
	req, err := http.NewRequestWithContext(ctx, "POST", g.ControlURL, strings.NewReader(b.String()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+g.ServiceType+"#"+action+`"`)
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	s := string(body)
	if resp.StatusCode/100 != 2 {
		e := &upnpError{}
		if m := soapErrCode.FindStringSubmatch(s); m != nil {
			fmt.Sscanf(m[1], "%d", &e.Code)
		}
		if m := soapErrDesc.FindStringSubmatch(s); m != nil {
			e.Desc = strings.TrimSpace(m[1])
		}
		if e.Code == 0 && e.Desc == "" {
			return "", fmt.Errorf("%s: HTTP %d", action, resp.StatusCode)
		}
		return "", e
	}
	return s, nil
}

var extIPRe = regexp.MustCompile(`<NewExternalIPAddress>([^<]*)</NewExternalIPAddress>`)

func (g *igd) externalIP(ctx context.Context) (string, error) {
	body, err := g.call(ctx, "GetExternalIPAddress", nil)
	if err != nil {
		return "", err
	}
	m := extIPRe.FindStringSubmatch(body)
	if m == nil {
		return "", errors.New("GetExternalIPAddress: no address in reply")
	}
	return strings.TrimSpace(m[1]), nil
}

// addMapping opens external port → lanIP:port for lease seconds (0 =
// permanent). Routers that only do permanent leases say so with error
// 725; we take the hint and ask again for one.
func (g *igd) addMapping(ctx context.Context, lanIP string, port int, lease uint32) (uint32, error) {
	args := func(lease uint32) [][2]string {
		return [][2]string{
			{"NewRemoteHost", ""},
			{"NewExternalPort", fmt.Sprint(port)},
			{"NewProtocol", "TCP"},
			{"NewInternalPort", fmt.Sprint(port)},
			{"NewInternalClient", lanIP},
			{"NewEnabled", "1"},
			{"NewPortMappingDescription", "fog"},
			{"NewLeaseDuration", fmt.Sprint(lease)},
		}
	}
	_, err := g.call(ctx, "AddPortMapping", args(lease))
	var ue *upnpError
	if errors.As(err, &ue) && ue.Code == 718 {
		// ConflictInMappingEntry: the external port is already mapped —
		// to this host's previous address after a DHCP renumbering, or
		// to whatever held it before fog. Ours to take: drop it and retry.
		if derr := g.deleteMapping(ctx, port); derr == nil {
			_, err = g.call(ctx, "AddPortMapping", args(lease))
		}
	}
	if errors.As(err, &ue) && ue.Code == 725 && lease != 0 {
		if _, err := g.call(ctx, "AddPortMapping", args(0)); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return lease, nil
}

func (g *igd) deleteMapping(ctx context.Context, port int) error {
	_, err := g.call(ctx, "DeletePortMapping", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", fmt.Sprint(port)},
		{"NewProtocol", "TCP"},
	})
	return err
}
