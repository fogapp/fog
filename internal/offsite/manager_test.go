package offsite

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/store/storetest"
)

type harness struct {
	m    *Manager
	fake *fakes3.Server
	url  string
	st   *store.Store
}

func newHarness(t *testing.T, fake *fakes3.Server) *harness {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	st, err := storetest.Open(filepath.Join(t.TempDir(), "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake.AccessKey = "key-1"
	m := &Manager{DataDir: t.TempDir(), Store: st, Vault: fakeVault{1}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Passphrase: func() string { return "passphrase 12345" }, ServerName: func() string { return "house" },
		HTTPClient: srv.Client(), DiskSpace: plentyOfRoom}
	return &harness{m: m, fake: fake, url: srv.URL, st: st}
}

func (h *harness) spec(name string) TargetSpec {
	return TargetSpec{Name: name, Provider: "b2", Endpoint: h.url, Region: "us-west-004", Bucket: h.fake.Bucket, Prefix: "backups", PathStyle: true, PricePerTB: 6.95}
}

func TestAddTargetGrades(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		shape func(f *fakes3.Server)
		grade string
	}{
		{"immutable", func(f *fakes3.Server) { f.Lock = fakes3.LockConfig{Enabled: true} }, "A"},
		// Backblaze's console only makes keys that can delete: the lock is
		// what holds, and the probe proves it on a locked test object.
		{"locked, key can delete", func(f *fakes3.Server) { f.Lock = fakes3.LockConfig{Enabled: true}; f.CanDeleteVersions = true }, "A"},
		{"append-only", func(*fakes3.Server) {}, "B"},
		{"versioned", func(f *fakes3.Server) { f.CanDeleteVersions = true }, "C"},
		{"plain", func(f *fakes3.Server) { f.Versioning = false; f.CanDeleteVersions = true }, "D"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := fakes3.New("fog-test")
			tc.shape(fake)
			h := newHarness(t, fake)
			target, probe, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret")
			if err != nil {
				t.Fatalf("add: %v (probe %+v)", err, probe)
			}
			if target.Grade != tc.grade || probe.Grade != tc.grade {
				t.Fatalf("grade %s / %s, want %s: %+v", target.Grade, probe.Grade, tc.grade, probe.Steps)
			}
			if tc.grade != "A" && probe.Fix == "" {
				t.Fatal("a grade below A must say how to raise it")
			}
			if len(target.LockerID) != 32 {
				t.Fatalf("locker id %q", target.LockerID)
			}
			// The probe's scratch objects were hidden, and locker.json exists.
			c, _ := h.m.Client(target)
			n := 0
			c.List(context.Background(), "backups/fog-locker/1/", func(o objstore.Object) error { n++; return nil })
			if n != 1 {
				t.Fatalf("expected only locker.json to remain visible, found %d objects", n)
			}
			// The stored view never carries the credentials.
			if keys, _ := h.st.SecretKeys(0); len(keys) != 2 {
				t.Fatalf("secrets: %v", keys)
			}
		})
	}
}

func TestAddTargetRefusesAnUnusableBucket(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fakes3.New("fog-test"))
	_, probe, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "wrong-key", "secret")
	if !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("expected ErrProbeFailed, got %v", err)
	}
	if probe == nil || probe.Steps[0].OK {
		t.Fatalf("the write step should have failed: %+v", probe)
	}
	if list, _ := h.st.ListOffsiteTargets(); len(list) != 0 {
		t.Fatal("a refused target must not be stored")
	}
	if keys, _ := h.st.SecretKeys(0); len(keys) != 0 {
		t.Fatalf("no credentials may remain: %v", keys)
	}
}

func TestAddTargetNeedsThePassphrase(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fakes3.New("fog-test"))
	h.m.Passphrase = func() string { return "" }
	if _, _, err := h.m.AddTarget(context.Background(), h.spec("cloud"), "key-1", "secret"); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("got %v", err)
	}
}

// A locker written by another machine (no wrap for this vault) is adopted
// with the passphrase and rewrapped so the nightly door opens for free;
// one made under a different passphrase is refused.
func TestAddTargetAdoptsOrRefusesAnExistingLocker(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	ctx := context.Background()
	other := &Manager{Store: h.st, Vault: fakeVault{9}, Log: h.m.Log, Passphrase: h.m.Passphrase, ServerName: h.m.ServerName, HTTPClient: h.m.HTTPClient}
	c, _ := other.clientFor(h.spec("x"), "key-1", "secret")
	layout, theirs, err := other.createLocker(ctx, c, "backups")
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := theirs.SealBytes("doc", []byte("theirs"))

	target, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if target.LockerID != layout.ID {
		t.Fatalf("adopted %s, expected %s", target.LockerID, layout.ID)
	}
	lk, err := FetchLocker(ctx, c, layout)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := lk.OpenWithMaster(fakeVault{1})
	if err != nil {
		t.Fatalf("after adoption the locker must open with this vault: %v", err)
	}
	if got, _ := mine.OpenBytes("doc", sealed, 10); string(got) != "theirs" {
		t.Fatal("adopted ring must open what the other machine sealed")
	}

	// Different passphrase → refused, and nothing stored.
	fake2 := fakes3.New("fog-test")
	h2 := newHarness(t, fake2)
	stranger := &Manager{Store: h2.st, Vault: fakeVault{9}, Log: h2.m.Log, Passphrase: func() string { return "someone elses 123" }, HTTPClient: h2.m.HTTPClient}
	c2, _ := stranger.clientFor(h2.spec("x"), "key-1", "secret")
	if _, _, err := stranger.createLocker(ctx, c2, "backups"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h2.m.AddTarget(ctx, h2.spec("cloud"), "key-1", "secret"); !errors.Is(err, ErrLockerLocked) {
		t.Fatalf("expected ErrLockerLocked, got %v", err)
	}
	if list, _ := h2.st.ListOffsiteTargets(); len(list) != 0 {
		t.Fatal("refused target stored")
	}
}

func TestReprobeUpdateAndDelete(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	ctx := context.Background()
	target, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if target.Grade != "B" {
		t.Fatalf("grade %s", target.Grade)
	}
	// The operator enables Object Lock on the provider's side.
	fake.Lock = fakes3.LockConfig{Enabled: true}
	target, probe, err := h.m.Reprobe(ctx, "cloud")
	if err != nil || target.Grade != "A" || probe.Fix != "" {
		t.Fatalf("reprobe: %s %v %+v", target.Grade, err, probe)
	}
	// An edit that keeps the bucket keeps the locker; a moved prefix gets
	// its own.
	spec := h.spec("cloud")
	spec.PricePerTB = 5
	spec.Window = "01:00-07:00"
	updated, _, err := h.m.UpdateTarget(ctx, "cloud", spec, "", "")
	if err != nil || updated.LockerID != target.LockerID || updated.PricePerTB != 5 || updated.Window != "01:00-07:00" {
		t.Fatalf("update: %+v %v", updated, err)
	}
	spec.Prefix = "elsewhere"
	moved, _, err := h.m.UpdateTarget(ctx, "cloud", spec, "", "")
	if err != nil || moved.LockerID == target.LockerID || moved.Prefix != "elsewhere" {
		t.Fatalf("move: %+v %v", moved, err)
	}
	spec.Window = "25:00-07:00"
	if _, _, err := h.m.UpdateTarget(ctx, "cloud", spec, "", ""); err == nil {
		t.Fatal("bad window accepted")
	}
	if err := h.m.DeleteTarget("cloud"); err != nil {
		t.Fatal(err)
	}
	if keys, _ := h.st.SecretKeys(0); len(keys) != 0 {
		t.Fatalf("credentials must go with the target: %v", keys)
	}
	if err := h.m.DeleteTarget("cloud"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// The buckets were never emptied.
	n := 0
	c, _ := h.m.clientFor(h.spec("x"), "key-1", "secret")
	c.List(ctx, "", func(objstore.Object) error { n++; return nil })
	if n < 2 {
		t.Fatalf("deleting a target must not touch the bucket; %d objects left", n)
	}
}

func TestTargetSpecValidation(t *testing.T) {
	t.Parallel()

	bad := []TargetSpec{
		{Name: "Bad Name", Endpoint: "https://x", Bucket: "b"},
		{Name: "ok", Endpoint: "s3.example.com", Bucket: "b"},
		{Name: "ok", Endpoint: "https://s3.<region>.backblazeb2.com", Bucket: "b"},
		{Name: "ok", Endpoint: "https://x", Bucket: ""},
		{Name: "ok", Endpoint: "https://x", Bucket: "b", Provider: "dropbox"},
		{Name: "ok", Endpoint: "https://x", Bucket: "b", PricePerTB: -1},
		{Name: "ok", Endpoint: "https://x", Bucket: "b", Prefix: "a//b"},
	}
	for _, spec := range bad {
		if err := spec.normalize(); err == nil {
			t.Errorf("accepted %+v", spec)
		}
	}
	good := TargetSpec{Name: " House ", Endpoint: "https://x/", Bucket: "b", Prefix: "/backups/house/"}
	if err := good.normalize(); err != nil || good.Name != "house" || good.Endpoint != "https://x" || good.Prefix != "backups/house" || good.Provider != "s3" {
		t.Fatalf("normalize: %+v %v", good, err)
	}
}

// Changing the kit passphrase must move the locker with it: recovery on a
// fresh machine opens the locker with the passphrase the operator now
// has, not the one from before the change.
func TestPassphraseChangeRewrapsLockers(t *testing.T) {
	t.Parallel()

	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	ctx := context.Background()
	if _, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret"); err != nil {
		t.Fatal(err)
	}
	target, err := h.st.OffsiteTargetByName("cloud")
	if err != nil {
		t.Fatal(err)
	}
	// Give the locker a second machine's vault wrap too; it must survive.
	c, _ := h.m.Client(target)
	lk, _ := FetchLocker(ctx, c, LayoutOf(target))
	kr, err := lk.OpenWithMaster(h.m.Vault)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := FetchLocker(ctx, c, LayoutOf(target))
	if err := other.wrapAll(kr, "", fakeVault{9}); err != nil {
		t.Fatal(err)
	}
	lk.Keys = append(lk.Keys, other.Keys...)
	if err := h.m.putLocker(ctx, c, LayoutOf(target), lk); err != nil {
		t.Fatal(err)
	}

	h.m.Passphrase = func() string { return "a brand new passphrase" }
	if _, _, err := OpenForRecovery(ctx, h.spec("x"), "key-1", "secret", "a brand new passphrase", nil); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("before the rewrap the new passphrase should not open the locker: %v", err)
	}
	if err := h.m.PassphraseChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenForRecovery(ctx, h.spec("x"), "key-1", "secret", "a brand new passphrase", nil); err != nil {
		t.Fatalf("new passphrase: %v", err)
	}
	if _, _, err := OpenForRecovery(ctx, h.spec("x"), "key-1", "secret", "passphrase 12345", nil); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("old passphrase still opens: %v", err)
	}
	after, _ := FetchLocker(ctx, c, LayoutOf(target))
	if _, err := after.OpenWithMaster(h.m.Vault); err != nil {
		t.Fatalf("this vault: %v", err)
	}
	if _, err := after.OpenWithMaster(fakeVault{9}); err != nil {
		t.Fatalf("the other machine's wrap was dropped: %v", err)
	}
}

// plentyOfRoom is a disk with room for any check, whatever the machine
// running the tests has free.
func plentyOfRoom(string) (int64, int64, bool) { return 500 << 30, 1 << 40, true }
