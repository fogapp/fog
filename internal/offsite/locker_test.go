package offsite

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/backup"
	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/objstore/fakes3"
	"gitea.lan/matthew/fog/internal/store"
	"gitea.lan/matthew/fog/internal/vault"
	"golang.org/x/crypto/chacha20poly1305"
)

func TestLockerOpensWithEitherWrap(t *testing.T) {
	vault := fakeVault{3}
	l, kr, err := CreateLocker("house", "correct horse battery staple", vault)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Keys) != 2 {
		t.Fatalf("expected a passphrase wrap and a master wrap, got %+v", l.Keys)
	}
	raw, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseLocker(raw)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := kr.SealBytes("doc", []byte("payload"))

	byPass, err := parsed.OpenWithPassphrase("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := byPass.OpenBytes("doc", sealed, 100); string(got) != "payload" {
		t.Fatal("passphrase ring does not open what the creating ring sealed")
	}
	byMaster, err := parsed.OpenWithMaster(vault)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := byMaster.OpenBytes("doc", sealed, 100); string(got) != "payload" {
		t.Fatal("master ring does not open what the creating ring sealed")
	}
	if _, err := parsed.OpenWithPassphrase("wrong"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := parsed.OpenWithMaster(fakeVault{9}); !errors.Is(err, ErrNoMasterWrap) {
		t.Fatalf("another machine's vault: %v", err)
	}
	// Blob ids are stable across rings of the same locker.
	id1, n, _ := byPass.BlobID(strings.NewReader("photo bytes"))
	id2, _, _ := byMaster.BlobID(strings.NewReader("photo bytes"))
	if id1 != id2 || n != 11 || len(id1) != 64 {
		t.Fatalf("blob id: %s %s %d", id1, id2, n)
	}
	other, _, _ := testKeyring(t).BlobID(strings.NewReader("photo bytes"))
	if other == id1 {
		t.Fatal("blob ids must be keyed per locker")
	}
}

func TestRewrapChangesPassphraseNotData(t *testing.T) {
	l, kr, err := CreateLocker("house", "old passphrase 12", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Keys) != 1 {
		t.Fatalf("no master given: expected one wrap, got %d", len(l.Keys))
	}
	sealed, _ := kr.SealBytes("doc", []byte("payload"))
	oldSalt := l.Salt
	if err := l.Rewrap(kr, "new passphrase 12", fakeVault{1}); err != nil {
		t.Fatal(err)
	}
	if l.Salt == oldSalt {
		t.Fatal("a new passphrase must get a new salt")
	}
	if _, err := l.OpenWithPassphrase("old passphrase 12"); err == nil {
		t.Fatal("old passphrase must stop working")
	}
	kr2, err := l.OpenWithPassphrase("new passphrase 12")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := kr2.OpenBytes("doc", sealed, 100); string(got) != "payload" {
		t.Fatal("data sealed before the rewrap must still open")
	}
	if _, err := l.OpenWithMaster(fakeVault{1}); err != nil {
		t.Fatalf("the added master wrap must open: %v", err)
	}
}

func TestRotateKeepsHistoryAndBlobIDs(t *testing.T) {
	l, kr, err := CreateLocker("house", "passphrase 12345", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := kr.SealBytes("doc", []byte("old"))
	idBefore, _, _ := kr.BlobID(strings.NewReader("file"))
	if err := l.Rotate(kr, "passphrase 12345", nil); err != nil {
		t.Fatal(err)
	}
	if kr.CurrentKeyID() != 2 {
		t.Fatalf("current key %d", kr.CurrentKeyID())
	}
	after, _ := kr.SealBytes("doc", []byte("new"))
	if bytes.Equal(before[:headerSize], after[:headerSize]) {
		t.Fatal("headers should name different keys")
	}
	reopened, err := l.OpenWithPassphrase("passphrase 12345")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.OpenBytes("doc", before, 10); err != nil || string(got) != "old" {
		t.Fatalf("history under key 1: %q %v", got, err)
	}
	if got, err := reopened.OpenBytes("doc", after, 10); err != nil || string(got) != "new" {
		t.Fatalf("new under key 2: %q %v", got, err)
	}
	idAfter, _, _ := reopened.BlobID(strings.NewReader("file"))
	if idAfter != idBefore {
		t.Fatal("rotation must not change blob ids")
	}
	// A ring missing key 2 reports the object as written under an
	// unknown key, not as corrupt.
	only1 := newKeyring(l.ID, map[uint32][]byte{1: kr.roots[1]})
	if _, err := only1.OpenBytes("doc", after, 10); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
}

// locker.json is unsealed, so a stolen bucket key can rewrite its KDF cost.
// Derivation runs before any authentication, so an absurd memory or time is
// an OOM or an unbounded hang, not a wrong answer. Opening must refuse it at
// once rather than attempt it.
func TestLockerRefusesAbsurdKDFParameters(t *testing.T) {
	l, _, err := CreateLocker("house", "correct horse battery staple", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		kdf  KDFParams
	}{
		{"memory", KDFParams{Time: 4, Memory: 64 << 20 /* 64 GiB */, Threads: 1}},
		{"memory just over cap", KDFParams{Time: 4, Memory: 1 << 20 /* 1 GiB, over the 512 MiB cap */, Threads: 1}},
		{"time", KDFParams{Time: 1_000_000, Memory: 256 * 1024, Threads: 1}},
		{"threads", KDFParams{Time: 4, Memory: 256 * 1024, Threads: 255}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := *l
			tampered.KDF = tc.kdf
			if _, err := tampered.OpenWithPassphrase("correct horse battery staple"); err == nil || !strings.Contains(err.Error(), "safe limits") {
				t.Fatalf("absurd %s must be refused before any derivation, got %v", tc.name, err)
			}
		})
	}
	// The default cost still opens.
	if _, err := l.OpenWithPassphrase("correct horse battery staple"); err != nil {
		t.Fatalf("a locker at the default cost must still open: %v", err)
	}
}

// A key wrap is bound to its locker's id, so a bucket writer cannot splice a
// wrap from another locker (opened by the same vault or passphrase) into this
// one under a matching key id and have it accepted.
func TestKeyWrapsBoundToLocker(t *testing.T) {
	vault := fakeVault{7}
	const pass = "correct horse battery staple"
	la, _, err := CreateLocker("a", pass, vault)
	if err != nil {
		t.Fatal(err)
	}
	lb, _, err := CreateLocker("b", pass, vault)
	if err != nil {
		t.Fatal(err)
	}
	// Splice B's wraps (same key id 1) into A.
	master := func(l *Locker) string {
		for _, w := range l.Keys {
			if w.WrappedBy == "master" {
				return w.Ciphertext
			}
		}
		return ""
	}
	pw := func(l *Locker) string {
		for _, w := range l.Keys {
			if w.WrappedBy == "passphrase" {
				return w.Ciphertext
			}
		}
		return ""
	}
	spliced := &Locker{Format: la.Format, ID: la.ID, Name: la.Name, CreatedAt: la.CreatedAt, KDF: la.KDF, Salt: la.Salt,
		Keys: []KeyWrap{{ID: 1, WrappedBy: "master", Ciphertext: master(lb)}, {ID: 1, WrappedBy: "passphrase", Ciphertext: pw(lb)}}}
	if _, err := spliced.OpenWithMaster(vault); !errors.Is(err, ErrNoMasterWrap) {
		t.Fatalf("a master wrap from another locker must be refused, got %v", err)
	}
	if _, err := spliced.OpenWithPassphrase(pass); err == nil {
		t.Fatal("a passphrase wrap from another locker must be refused")
	}
	// A's own wraps still open A.
	if _, err := la.OpenWithMaster(vault); err != nil {
		t.Fatalf("A's own master wrap must still open: %v", err)
	}
}

func TestParseLockerRefusesOtherFormats(t *testing.T) {
	if _, err := ParseLocker([]byte(`{"format":"fog-locker/2","id":"x","keys":[{}]}`)); err == nil {
		t.Fatal("future format accepted")
	}
	if _, err := ParseLocker([]byte(`not json`)); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestLayoutNames(t *testing.T) {
	l := Layout{Prefix: "/backups/", ID: "0123456789abcdef0123456789abcdef"}
	if l.Root() != "backups/fog-locker/1/0123456789abcdef0123456789abcdef/" {
		t.Fatal(l.Root())
	}
	if LockersRoot("") != "fog-locker/1/" {
		t.Fatal(LockersRoot(""))
	}
	if got := l.Blob("abcdef0123"); got != l.Root()+"files/blobs/ab/abcdef0123" {
		t.Fatal(got)
	}
	if got := l.Name(l.AppSnapshot("hub", "20260827-010314")); got != "apps/hub/20260827-010314.snapshot" {
		t.Fatal(got)
	}
	for in, want := range map[string]string{
		l.AppRow("hub", "20260827-010314"):             "20260827-010314",
		l.AppSnapshot("hub", "20260827-010314-2"):      "20260827-010314-2",
		l.FilesSnapshot("20260827-010314-12"):          "20260827-010314-12",
		"kits/fog-recovery-kit-20260827-010314.enc":    "20260827-010314",
		"index/20260827-012000.json":                   "20260827-012000",
		"x/notastamp.json":                             "",
		"apps/hub/2026082-010314.snapshot":             "",
		"files/blobs/ab/abcdef20260827-010314deadbeef": "",
		"\t20260827-010314.json":                       "", // leading control: not anchored → rejected
		" 20260827-010314.json":                        "", // leading space
		"evil\x1b[31m-20260827-010314.json":            "", // ANSI escape in the prefix
	} {
		if got := StampOf(in); got != want {
			t.Errorf("StampOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// Two Files snapshots in one second are "<stamp>" and "<stamp>-2" on disk.
// The offsite name used to drop the suffix (falling back to the creation
// time), so both mapped to one key and the second overwrote the first.
func TestSameSecondSnapshotsKeepDistinctOffsiteKeys(t *testing.T) {
	created := "2026-08-27T01:03:14Z"
	a := stampOf("/backups/files/20260827-010314", created, 1)
	b := stampOf("/backups/files/20260827-010314-2", created, 2)
	if a != "20260827-010314" || b != "20260827-010314-2" {
		t.Fatalf("stamps %q, %q", a, b)
	}
	l := Layout{ID: "x"}
	if l.FilesSnapshot(a) == l.FilesSnapshot(b) || l.AppSnapshot("hub", a) == l.AppSnapshot("hub", b) {
		t.Fatal("same-second snapshots share an offsite key")
	}
	// Retention still places the suffixed one in its second.
	ta, okA := StampTime(StampOf(l.FilesSnapshot(a)))
	tb, okB := StampTime(StampOf(l.FilesSnapshot(b)))
	if !okA || !okB || !ta.Equal(tb) {
		t.Fatalf("stamp times %v %v, %v %v", ta, okA, tb, okB)
	}
}

// legacyLocker rewrites l's wraps in the pre-binding format — the shape every
// locker had before wraps named their locker — for the upgrade test.
func legacyLocker(t *testing.T, l *Locker, kr *Keyring, pass string, master vault.Cipher) *Locker {
	t.Helper()
	old := *l
	old.WrapFormat = 0
	old.Keys = nil
	kek, err := old.kek(pass)
	if err != nil {
		t.Fatal(err)
	}
	for id, root := range kr.roots {
		aead, _ := chacha20poly1305.NewX(kek)
		nonce := make([]byte, aead.NonceSize())
		rand.Read(nonce)
		ct := aead.Seal(nonce, nonce, root, legacyWrapAAD(id))
		old.Keys = append(old.Keys, KeyWrap{ID: id, WrappedBy: "passphrase", Ciphertext: base64.StdEncoding.EncodeToString(ct)})
		mct, _ := master.Encrypt(fmt.Sprintf("%d:%s", id, hex.EncodeToString(root)))
		old.Keys = append(old.Keys, KeyWrap{ID: id, WrappedBy: "master", Ciphertext: mct})
	}
	return &old
}

// An old-format locker (wraps that name no locker) is upgraded to the
// locker-bound format on the first sync, with no operator action, and the
// fact is recorded — in locker.json and in fog's own store — so that
// afterwards a spliced old-format wrap is refused even with the marker
// stripped from the file.
func TestLegacyLockerUpgradedOnSyncThenSpliceRefused(t *testing.T) {
	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	ctx := context.Background()
	target, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := h.m.Client(target)
	layout := LayoutOf(target)
	lk, err := FetchLocker(ctx, c, layout)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := lk.OpenWithMaster(h.m.Vault)
	if err != nil {
		t.Fatal(err)
	}
	// Turn the bucket's locker back into an old-format one, as a fog from
	// before the binding left it, and forget that fog ever upgraded it.
	put := func(l *Locker) {
		raw, _ := l.Marshal()
		if _, err := c.PutBytes(ctx, layout.LockerJSON(), raw, objstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		h.m.forgetRing("cloud")
	}
	put(legacyLocker(t, lk, kr, "passphrase 12345", h.m.Vault))
	h.st.DeleteSetting(boundSetting(lk.ID))

	// The first sync after the upgrade rewrites every wrap in bound form.
	if err := runSync(t, h, "cloud"); err != nil {
		t.Fatal(err)
	}
	after, err := FetchLocker(ctx, c, layout)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Bound() {
		t.Fatal("the first sync must mark the locker bound in locker.json")
	}
	for _, w := range after.Keys {
		if w.WrappedBy != "master" {
			continue
		}
		if plain, err := h.m.Vault.Decrypt(w.Ciphertext); err == nil && !strings.HasPrefix(plain, lk.ID+":") {
			t.Fatalf("a master wrap was left in the old format: %q", plain)
		}
	}
	if v, _ := h.st.GetSetting(boundSetting(lk.ID)); v != "1" {
		t.Fatal("fog must record the upgrade in its own store")
	}

	// Splice: another locker's old-format master wrap, under the same key id,
	// with the bound marker stripped from the file to try to roll back.
	_, okr, err := CreateLocker("other", "passphrase 12345", h.m.Vault)
	if err != nil {
		t.Fatal(err)
	}
	spliceCT, _ := h.m.Vault.Encrypt(fmt.Sprintf("1:%s", hex.EncodeToString(okr.roots[1])))
	spliced := *after
	spliced.WrapFormat = 0
	spliced.Keys = []KeyWrap{{ID: 1, WrappedBy: "master", Ciphertext: spliceCT}}
	// A reader that knows the locker is bound refuses the old-format wrap.
	enforced := spliced
	h.m.enforceBound(&enforced)
	if _, err := enforced.OpenWithMaster(h.m.Vault); !errors.Is(err, ErrNoMasterWrap) {
		t.Fatalf("a spliced old-format wrap must be refused once the locker is bound, got %v", err)
	}
	// End to end: fog will not open the locker with the spliced key...
	put(&spliced)
	if _, err := h.m.Keyring(ctx, target); err == nil {
		t.Fatal("fog must not open a locker whose only wrap is a spliced old-format one")
	}
	// ...and verify's re-check names the rollback.
	if err := h.m.recheckLocker(ctx, target); err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("verify must flag a locker.json that lost its bound marker, got %v", err)
	}
}

// Verify used to try the passphrase only when the vault wrap was missing, so
// stripped or corrupted passphrase wraps passed every check while fog coasted
// on the vault door — and fresh-machine recovery then failed. Every verify
// now opens with the stored passphrase too, and alerts on any failure.
func TestVerifyCatchesStrippedPassphraseWraps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(l *Locker)
	}{
		{"stripped", func(l *Locker) {
			var keep []KeyWrap
			for _, w := range l.Keys {
				if w.WrappedBy != "passphrase" {
					keep = append(keep, w)
				}
			}
			l.Keys = keep
		}},
		{"corrupted", func(l *Locker) {
			for i, w := range l.Keys {
				if w.WrappedBy == "passphrase" {
					raw, _ := base64.StdEncoding.DecodeString(w.Ciphertext)
					raw[len(raw)-1] ^= 0xff
					l.Keys[i].Ciphertext = base64.StdEncoding.EncodeToString(raw)
				}
			}
		}},
		{"rewrapped under another passphrase", func(l *Locker) {
			c := *l
			kr, err := c.OpenWithMaster(fakeVault{1})
			if err != nil {
				panic(err)
			}
			c.Salt = base64.StdEncoding.EncodeToString(make([]byte, saltSize))
			c.wrapAll(kr, "not the operator's passphrase", fakeVault{1})
			*l = c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := fakes3.New("fog-test")
			h := newHarness(t, fake)
			ctx := context.Background()
			target, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret")
			if err != nil {
				t.Fatal(err)
			}
			if err := h.m.recheckLocker(ctx, target); err != nil {
				t.Fatalf("an intact locker must pass: %v", err)
			}
			c, _ := h.m.Client(target)
			lk, _ := FetchLocker(ctx, c, LayoutOf(target))
			tc.tamper(lk)
			raw, _ := lk.Marshal()
			if _, err := c.PutBytes(ctx, LayoutOf(target).LockerJSON(), raw, objstore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := h.m.recheckLocker(ctx, target); err == nil || !strings.Contains(err.Error(), "passphrase") {
				t.Fatalf("verify must flag passphrase wraps a fresh machine could not use: %v", err)
			}
		})
	}
}

// A decoy kit planted to sort newest in the bucket — junk, or an old kit
// renamed — must not block recovery: OpenNewestKit skips it, says so, and
// opens the newest real kit. And verify notices a newest kit fog never
// uploaded.
func TestDecoyKitSkippedAndFlagged(t *testing.T) {
	fake := fakes3.New("fog-test")
	h := newHarness(t, fake)
	ctx := context.Background()
	target, _, err := h.m.AddTarget(ctx, h.spec("cloud"), "key-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.m.Reader(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	// A real kit, uploaded by fog (recorded as a copy), and a decoy newer
	// than it that fog never uploaded.
	realKit := realKitBytes(t, "20260827-010000", "passphrase 12345")
	r.Client.PutBytes(ctx, r.Layout.Kit("fog-recovery-kit-20260827-010000.enc"), realKit, objstore.PutOptions{})
	h.st.UpsertOffsiteCopy(store.OffsiteCopy{TargetID: target.ID, Kind: store.OffsiteKindKit, Subject: "fog-recovery-kit-20260827-010000.enc",
		Key: r.Layout.Kit("fog-recovery-kit-20260827-010000.enc"), State: store.OffsiteStored})
	r.Client.PutBytes(ctx, r.Layout.Kit("fog-recovery-kit-20991231-235959.enc"), []byte("FOGKIT2\x00not really a kit at all, but long enough"), objstore.PutOptions{})

	kit, err := OpenNewestKit(ctx, r, "passphrase 12345")
	if err != nil {
		t.Fatalf("a decoy must not block recovery: %v", err)
	}
	if kit.Name != "fog-recovery-kit-20260827-010000.enc" || len(kit.Skipped) != 1 || kit.Skipped[0].Name != "fog-recovery-kit-20991231-235959.enc" {
		t.Fatalf("want the real kit, with the decoy reported skipped: %s %+v", kit.Name, kit.Skipped)
	}
	if err := h.m.recheckKits(ctx, target); err == nil {
		t.Fatal("verify must flag a newest kit fog did not upload")
	}
}

// realKitBytes is a sealed current-format kit carrying its stamp.
func realKitBytes(t *testing.T, stamp, pass string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"master.key": "mk", "fog.db": "db",
		"kit-meta.json": `{"format":"fog-recovery-kit/1","stamp":"` + stamp + `","fog":"house","created_at":"2026-08-27T01:00:00Z"}`} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	sealed, err := backup.EncryptKit(buf.Bytes(), pass)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}
