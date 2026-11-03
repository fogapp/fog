package backup

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeArtifact builds a fog-backup/1 artifact whose manifest describes the
// components it actually contains, then applies tweak to the manifest so a
// test can make exactly one thing wrong.
func writeArtifact(t *testing.T, components map[string]string, tweak func(m *Manifest)) string {
	t.Helper()
	m := &Manifest{Format: ArtifactFormat, App: "hub", CreatedAt: "2026-08-17T00:00:00Z"}
	m.ProducedBy.GitSHA = "abc123"
	m.ProducedBy.SchemaVersion = "20260810000000"
	for path, body := range components {
		sum := sha256.Sum256([]byte(body))
		m.Components = append(m.Components, ManifestComponent{
			Name: filepath.Base(path), Path: path, Kind: "db",
			Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:]),
		})
	}
	if tweak != nil {
		tweak(m)
	}

	path := filepath.Join(t.TempDir(), "artifact.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	// Manifest first, as the contract requires.
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", string(raw))
	for p, body := range components {
		write(p, body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// rawArtifact writes a tar of exactly the (name, body) entries given, in
// order, so a test can plant a duplicate or ambiguous entry the normal
// builder would never produce.
func rawArtifact(t *testing.T, entries [][2]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o600, Size: int64(len(e[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// An artifact with two entries that name the same thing is ambiguous: the
// checksum pass reads one copy and a restore might read the other. Every
// reader must refuse it, whether the collision is a literal duplicate or an
// ambiguity like "./x" vs "x".
func TestArtifactRefusesDuplicateEntries(t *testing.T) {
	manifest := `{"format":"fog-backup/1","app":"hub"}`
	cases := map[string][][2]string{
		"duplicate component":  {{"manifest.json", manifest}, {"db/primary.dump", "a"}, {"db/primary.dump", "b"}},
		"ambiguous ./ vs bare": {{"manifest.json", manifest}, {"db/primary.dump", "a"}, {"./db/primary.dump", "b"}},
		"duplicate manifest":   {{"manifest.json", manifest}, {"manifest.json", manifest}, {"db/primary.dump", "a"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			path := rawArtifact(t, entries)
			// scanArtifact (preflight, files-only verify) refuses it.
			if _, _, err := scanArtifact(path); err == nil || !strings.Contains(err.Error(), "more than one") {
				t.Errorf("scanArtifact: want a duplicate-entry refusal, got %v", err)
			}
			// extractEntry (the dump extraction the DB verify uses) refuses it.
			if _, err := extractEntry(mustOpen(t, path), "db/primary.dump", t.TempDir(), 1<<40); err == nil || !strings.Contains(err.Error(), "more than one") {
				t.Errorf("extractEntry: want a duplicate-entry refusal, got %v", err)
			}
			// readManifest refuses a second manifest.json.
			if name == "duplicate manifest" {
				if _, err := readManifest(path); err == nil || !strings.Contains(err.Error(), "more than one") {
					t.Errorf("readManifest: want a duplicate-manifest refusal, got %v", err)
				}
			}
		})
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// secrets returns a reader over fixed values, standing in for the vault.
func secrets(kv map[string]string) func(string) string {
	return func(key string) string { return kv[key] }
}

func hasError(res *PreflightResult, substr string) bool {
	for _, e := range res.Errors {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func TestPreflightAccepts(t *testing.T) {
	path := writeArtifact(t, map[string]string{"db/primary.dump": "rows"}, func(m *Manifest) {
		m.SecretsFingerprint = map[string]string{"SECRET_KEY_BASE": fingerprint("s3cret")}
	})
	res := PreflightArtifact(path, "hub", "hub", secrets(map[string]string{"SECRET_KEY_BASE": "s3cret"}), false, false)
	if !res.OK() {
		t.Fatalf("a sound artifact was refused: %v", res.Errors)
	}
	if res.Manifest == nil || res.Manifest.App != "hub" {
		t.Fatal("manifest not reported back to the caller")
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
}

// The secrets check is the one that decides whether restored rows decrypt, and
// the one an operator can still act on at staging time.
func TestPreflightSecrets(t *testing.T) {
	path := writeArtifact(t, map[string]string{"db/primary.dump": "rows"}, func(m *Manifest) {
		m.SecretsFingerprint = map[string]string{
			"SECRET_KEY_BASE":           fingerprint("from-source"),
			"AR_ENCRYPTION_PRIMARY_KEY": fingerprint("also-from-source"),
		}
	})

	// A fresh install's generated values: present, but not the ones the data
	// was written with.
	res := PreflightArtifact(path, "hub", "hub", secrets(map[string]string{
		"SECRET_KEY_BASE": "freshly-generated", "AR_ENCRYPTION_PRIMARY_KEY": "also-generated",
	}), false, false)
	if res.OK() {
		t.Fatal("mismatched encryption secrets were accepted")
	}
	if !hasError(res, "SECRET_KEY_BASE differs") || !hasError(res, "AR_ENCRYPTION_PRIMARY_KEY differs") {
		t.Fatalf("both mismatches should be reported, got %v", res.Errors)
	}

	// Absent entirely reads as a refusal too, not a pass.
	res = PreflightArtifact(path, "hub", "hub", secrets(nil), false, false)
	if res.OK() || !hasError(res, "is not set here") {
		t.Fatalf("missing secrets: %v", res.Errors)
	}
	if res := PreflightArtifact(path, "hub", "hub", nil, false, false); res.OK() {
		t.Fatal("a nil secret reader must refuse, not skip the check")
	}

	// A server rebuilt without its recovery kit: the operator restores
	// knowing the old secrets are gone. Each mismatch and absence becomes a
	// warning naming the key; nothing else about the verdict softens.
	res = PreflightArtifact(path, "hub", "hub", secrets(map[string]string{"SECRET_KEY_BASE": "freshly-generated"}), false, true)
	if !res.OK() {
		t.Fatalf("forgetSecrets still refused: %v", res.Errors)
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "SECRET_KEY_BASE differs") || !strings.Contains(joined, "AR_ENCRYPTION_PRIMARY_KEY is not set here") {
		t.Fatalf("forgotten secrets should each be a warning, got %v", res.Warnings)
	}
	if res := PreflightArtifact(path, "manor", "manor", secrets(nil), false, true); res.OK() {
		t.Fatal("forgetSecrets must not excuse an artifact for another app")
	}
}

func TestPreflightRefusals(t *testing.T) {
	good := map[string]string{"db/primary.dump": "rows"}

	// Identity is the software, never fog's label: a hub artifact restores
	// into an app called anything as long as that app runs hub.
	t.Run("wrong software", func(t *testing.T) {
		res := PreflightArtifact(writeArtifact(t, good, nil), "hub-migrated", "manor", secrets(nil), false, false)
		if !hasError(res, `artifact is for "hub" but hub-migrated runs "manor"`) {
			t.Fatalf("got %v", res.Errors)
		}
	})
	t.Run("any name for the same software", func(t *testing.T) {
		res := PreflightArtifact(writeArtifact(t, good, nil), "hub-lxc", "hub", secrets(nil), false, false)
		if hasError(res, "is for") {
			t.Fatalf("a hub artifact belongs in any hub, got %v", res.Errors)
		}
	})
	t.Run("unknown software warns and defers to the app", func(t *testing.T) {
		res := PreflightArtifact(writeArtifact(t, good, nil), "hub-new", "", secrets(nil), false, false)
		if hasError(res, "is for") {
			t.Fatalf("no identity is a warning, not a refusal, got %v", res.Errors)
		}
		if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), `the artifact is for "hub"`) {
			t.Fatalf("expected a warning naming the artifact's app, got %v", res.Warnings)
		}
	})

	t.Run("wrong format", func(t *testing.T) {
		path := writeArtifact(t, good, func(m *Manifest) { m.Format = "fog-backup/2" })
		res := PreflightArtifact(path, "hub", "hub", secrets(nil), false, false)
		if !hasError(res, "declares format") {
			t.Fatalf("got %v", res.Errors)
		}
	})

	t.Run("corrupt component", func(t *testing.T) {
		path := writeArtifact(t, good, func(m *Manifest) { m.Components[0].SHA256 = strings.Repeat("0", 64) })
		res := PreflightArtifact(path, "hub", "hub", secrets(nil), false, false)
		if !hasError(res, "fails checksum verification") {
			t.Fatalf("got %v", res.Errors)
		}
	})

	t.Run("missing component", func(t *testing.T) {
		path := writeArtifact(t, good, func(m *Manifest) {
			m.Components = append(m.Components, ManifestComponent{Path: "db/gone.dump", Bytes: 1, SHA256: "x"})
		})
		res := PreflightArtifact(path, "hub", "hub", secrets(nil), false, false)
		if !hasError(res, "missing from the artifact") {
			t.Fatalf("got %v", res.Errors)
		}
	})

	t.Run("not a tar", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "artifact.tar")
		if err := os.WriteFile(path, []byte("not a tar at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := PreflightArtifact(path, "hub", "hub", secrets(nil), false, false)
		if res.OK() {
			t.Fatal("garbage was accepted as an artifact")
		}
	})

	t.Run("absent file", func(t *testing.T) {
		res := PreflightArtifact(filepath.Join(t.TempDir(), "nope.tar"), "hub", "hub", secrets(nil), false, false)
		if !hasError(res, "no artifact at") {
			t.Fatalf("got %v", res.Errors)
		}
	})
}

// A host-bound artifact is a warning the operator confirms, never a silent
// pass and never an unconditional refusal.
func TestPreflightIdentity(t *testing.T) {
	path := writeArtifact(t, map[string]string{"db/primary.dump": "rows"}, func(m *Manifest) {
		m.Identity.Host = "hub.old-server.lan"
	})

	res := PreflightArtifact(path, "hub", "hub", secrets(nil), false, false)
	if res.OK() || !hasError(res, "confirm the identity change") {
		t.Fatalf("unconfirmed identity change: %v", res.Errors)
	}

	res = PreflightArtifact(path, "hub", "hub", secrets(nil), true, false)
	if !res.OK() {
		t.Fatalf("confirmed identity change was still refused: %v", res.Errors)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "hub.old-server.lan") {
		t.Fatalf("confirmation should leave a warning, got %v", res.Warnings)
	}
}

func TestInspectArtifact(t *testing.T) {
	path := writeArtifact(t, map[string]string{"db/primary.dump": "rows"}, func(m *Manifest) {
		m.SecretsFingerprint = map[string]string{"SECRET_KEY_BASE": fingerprint("s3cret")}
	})
	m, err := InspectArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.App != "hub" || m.ProducedBy.GitSHA != "abc123" {
		t.Fatalf("manifest fields: %+v", m)
	}
	if m.SecretsFingerprint["SECRET_KEY_BASE"] != fingerprint("s3cret") {
		t.Fatal("fingerprints must reach the install form")
	}

	// Inspect is also the gate that tells a .snapshot.tar apart from an
	// artifact — the mistake that started all this.
	snapshot := filepath.Join(t.TempDir(), "hub.snapshot.tar")
	f, _ := os.Create(snapshot)
	tw := tar.NewWriter(f)
	tw.WriteHeader(&tar.Header{Name: "db-hub_production.dump", Mode: 0o600, Size: 4})
	tw.Write([]byte("rows"))
	tw.Close()
	f.Close()
	if _, err := InspectArtifact(snapshot); err == nil {
		t.Fatal("a volume snapshot bundle must not pass as an artifact")
	}
}

// The duplicate-entry check must not itself be a way to exhaust memory: a
// tar of endless entries, or names megabytes long (PAX allows it), is refused
// at a bound rather than remembered whole.
func TestArtifactEntryChecksAreBounded(t *testing.T) {
	// Past the entry limit.
	many := make([][2]string, 0, maxArtifactEntries+2)
	many = append(many, [2]string{"manifest.json", `{"format":"fog-backup/1","app":"hub"}`})
	for i := 0; i <= maxArtifactEntries; i++ {
		many = append(many, [2]string{fmt.Sprintf("f/%d", i), ""})
	}
	path := rawArtifact(t, many)
	if _, _, err := scanArtifact(path); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("scanArtifact: want an entry-count refusal, got %v", err)
	}
	if _, err := extractEntry(mustOpen(t, path), "db/primary.dump", t.TempDir(), 1<<40); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("extractEntry: want an entry-count refusal, got %v", err)
	}
	if _, err := readManifest(path); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("readManifest: want an entry-count refusal, got %v", err)
	}

	// A name past the length limit (written as a PAX long name).
	long := rawArtifact(t, [][2]string{{"manifest.json", `{}`}, {strings.Repeat("a", maxEntryNameBytes+1), "x"}})
	if _, _, err := scanArtifact(long); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("scanArtifact: want a name-length refusal, got %v", err)
	}
	if _, err := extractEntry(mustOpen(t, long), "db/primary.dump", t.TempDir(), 1<<40); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("extractEntry: want a name-length refusal, got %v", err)
	}
}

// sparseTar is a tar whose db/primary.dump is stored PAX-sparse (format
// 1.0): a few bytes in the archive that expand to size on reading. Go's
// writer will not produce one, so the blocks are laid out by hand.
func sparseTar(t *testing.T, size int64) string {
	t.Helper()
	block := func(name string, typ byte, n int) []byte {
		h := make([]byte, 512)
		copy(h, name)
		copy(h[100:], "0000600\x00")
		copy(h[108:], "0000000\x00")
		copy(h[116:], "0000000\x00")
		copy(h[124:], fmt.Sprintf("%011o\x00", n))
		copy(h[136:], "00000000000\x00")
		h[156] = typ
		copy(h[257:], "ustar\x0000")
		for i := 148; i < 156; i++ {
			h[i] = ' '
		}
		sum := 0
		for _, c := range h {
			sum += int(c)
		}
		copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
		return h
	}
	pad := func(b []byte) []byte { return append(b, make([]byte, (512-len(b)%512)%512)...) }
	rec := func(k, v string) string {
		line := " " + k + "=" + v + "\n"
		n := len(line) + 1
		for len(strconv.Itoa(n))+len(line) != n {
			n++
		}
		return strconv.Itoa(n) + line
	}
	pax := rec("GNU.sparse.major", "1") + rec("GNU.sparse.minor", "0") +
		rec("GNU.sparse.name", "db/primary.dump") + rec("GNU.sparse.realsize", strconv.FormatInt(size, 10))
	body := pad([]byte("1\n0\n4\n"))
	body = append(body, []byte("PGDM")...)
	var out []byte
	out = append(out, block("PaxHeaders/primary.dump", 'x', len(pax))...)
	out = append(out, pad([]byte(pax))...)
	out = append(out, block("GNUSparseFile.0/primary.dump", '0', len(body))...)
	out = append(out, pad(body)...)
	out = append(out, make([]byte, 1024)...)
	p := filepath.Join(t.TempDir(), "artifact.tar")
	os.WriteFile(p, out, 0o600)
	return p
}

// A 10 KB artifact whose dump is stored sparse expanded to gigabytes on
// disk — and the restore's time budget grew with it. Sparse entries are
// refused, and no entry may claim more bytes than the artifact holds.
func TestSparseDumpIsRefused(t *testing.T) {
	p := sparseTar(t, 2<<30)
	fi, _ := os.Stat(p)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out, err := extractEntry(f, "db/primary.dump", t.TempDir(), fi.Size())
	if err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("a sparse dump was extracted to %s: %v", out, err)
	}
	if _, _, err := scanTar(mustOpen(t, p)); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("the checksum scan must refuse a sparse entry: %v", err)
	}
}
