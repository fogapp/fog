package issues

import (
	"context"
	"errors"
	"net"
	"testing"
)

// A host probe that fails is no evidence: the check must leave the host
// unevaluated, or the sweep resolves the open issue and the next good
// sweep re-files and re-notifies it.
func TestNetworkChecksSkipWhenProbeFails(t *testing.T) {
	swapHost, swapLookup, swapIfaces := hostname, lookupHost, netInterfaces
	t.Cleanup(func() { hostname, lookupHost, netInterfaces = swapHost, swapLookup, swapIfaces })
	ctx := context.Background()

	hostname = func() (string, error) { return "", errors.New("uts namespace gone") }
	if rep := checkMdnsCollision(ctx, nil); len(rep.evaluated) != 0 {
		t.Errorf("hostname failure must not evaluate the host, got %+v", rep)
	}

	hostname = func() (string, error) { return "fog", nil }
	lookupHost = func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "i/o timeout", Name: "fog.local", IsTimeout: true}
	}
	if rep := checkMdnsCollision(ctx, nil); len(rep.evaluated) != 0 {
		t.Errorf("a resolver timeout must not evaluate the host, got %+v", rep)
	}

	// No answer at all is an answer: nobody else owns the name.
	lookupHost = func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "fog.local", IsNotFound: true}
	}
	if rep := checkMdnsCollision(ctx, nil); len(rep.evaluated) != 1 || len(rep.findings) != 0 {
		t.Errorf("an unanswered name evaluates the host clean, got %+v", rep)
	}

	lookupHost = func(context.Context, string) ([]string, error) { return []string{"192.0.2.77"}, nil }
	if rep := checkMdnsCollision(ctx, nil); len(rep.evaluated) != 1 || len(rep.findings) != 1 {
		t.Errorf("a foreign answer files the collision, got %+v", rep)
	}

	netInterfaces = func() ([]net.Interface, error) { return nil, errors.New("netlink: permission denied") }
	if rep := checkDockerSubnetOverlap(ctx, nil); len(rep.evaluated) != 0 {
		t.Errorf("an unreadable interface table must not evaluate the host, got %+v", rep)
	}
	netInterfaces = func() ([]net.Interface, error) { return nil, nil }
	if rep := checkDockerSubnetOverlap(ctx, nil); len(rep.evaluated) != 1 || len(rep.findings) != 0 {
		t.Errorf("a readable table with no overlap evaluates the host clean, got %+v", rep)
	}
}

func TestHostEnvSkipsWhenBinaryUnknown(t *testing.T) {
	swap := executable
	t.Cleanup(func() { executable = swap })
	sn := &snapshot{dataDir: t.TempDir()}

	executable = func() (string, error) { return "", errors.New("no /proc") }
	if rep := (&Scanner{}).checkHostEnv(context.Background(), sn); len(rep.evaluated) != 0 {
		t.Fatalf("an unlocatable binary must not evaluate the host, got %+v", rep)
	}
	bin := t.TempDir()
	executable = func() (string, error) { return bin + "/fogd", nil }
	if rep := (&Scanner{}).checkHostEnv(context.Background(), sn); len(rep.evaluated) != 1 || len(rep.findings) != 0 {
		t.Fatalf("writable dirs evaluate the host clean, got %+v", rep)
	}
}
