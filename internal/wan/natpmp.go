package wan

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// NAT-PMP (RFC 6886): the small UDP protocol Apple routers and most
// consumer gateways speak on port 5351. Two requests matter here — "what
// is your external address" and "map this TCP port to me" — and both fit
// in a dozen bytes.

const natpmpPort = 5351

// natpmpPortOverride lets tests stand a fake router up on loopback; zero
// means the real port.
var natpmpPortOverride int

func natpmpDial(gw net.IP) (*net.UDPConn, error) {
	port := natpmpPort
	if natpmpPortOverride != 0 {
		port = natpmpPortOverride
	}
	return net.DialUDP("udp4", nil, &net.UDPAddr{IP: gw, Port: port})
}

var natpmpResults = map[uint16]string{
	1: "unsupported version",
	2: "refused — NAT-PMP is disabled on the router",
	3: "network failure — the router has no external address yet",
	4: "out of resources on the router",
	5: "unsupported opcode",
}

// natpmpExchange sends one request and waits for the matching response,
// retransmitting with doubling waits the way the RFC asks. Four tries is
// under four seconds — enough for any router that speaks the protocol,
// short enough that a silent one does not stall the caller.
func natpmpExchange(ctx context.Context, gw net.IP, req []byte, wantOp byte, wantLen int) ([]byte, error) {
	conn, err := natpmpDial(gw)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	wait := 250 * time.Millisecond
	buf := make([]byte, 64)
	for try := 0; try < 4; try++ {
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(wait)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		if err == nil && n >= wantLen && buf[0] == 0 && buf[1] == wantOp {
			if code := binary.BigEndian.Uint16(buf[2:4]); code != 0 {
				msg := natpmpResults[code]
				if msg == "" {
					msg = fmt.Sprintf("result code %d", code)
				}
				return nil, errors.New(msg)
			}
			return buf[:n], nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		wait *= 2
	}
	return nil, errors.New("no answer from the router")
}

// natpmpExternalIP asks the gateway for its WAN address.
func natpmpExternalIP(ctx context.Context, gw net.IP) (string, error) {
	resp, err := natpmpExchange(ctx, gw, []byte{0, 0}, 128, 12)
	if err != nil {
		return "", err
	}
	return net.IPv4(resp[8], resp[9], resp[10], resp[11]).String(), nil
}

// natpmpMapTCP maps external port → this host's same port for lifetime
// seconds. Returns the lifetime the router actually granted (routers may
// shorten it).
func natpmpMapTCP(ctx context.Context, gw net.IP, port int, lifetime uint32) (uint32, error) {
	req := make([]byte, 12)
	req[1] = 2 // opcode: map TCP
	binary.BigEndian.PutUint16(req[4:6], uint16(port))
	binary.BigEndian.PutUint16(req[6:8], uint16(port))
	binary.BigEndian.PutUint32(req[8:12], lifetime)
	resp, err := natpmpExchange(ctx, gw, req, 130, 16)
	if err != nil {
		return 0, err
	}
	if got := int(binary.BigEndian.Uint16(resp[10:12])); got != port {
		// The router handed us a different external port. Useless for a
		// hostname that must answer on 443 — treat as failure and release it.
		rel := make([]byte, 12)
		rel[1] = 2
		binary.BigEndian.PutUint16(rel[4:6], uint16(port))
		natpmpExchange(ctx, gw, rel, 130, 16)
		return 0, fmt.Errorf("router offered external port %d instead of %d — something else already holds it", got, port)
	}
	return binary.BigEndian.Uint32(resp[12:16]), nil
}
