package supervise

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// minimalELF builds the smallest header debug/elf will parse: a 64-bit
// little-endian executable with no sections or segments, declaring the
// given machine.
func minimalELF(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4] = 2                                       // ELFCLASS64
	b[5] = 1                                       // little-endian
	b[6] = 1                                       // EV_CURRENT
	binary.LittleEndian.PutUint16(b[16:], 2)       // e_type: ET_EXEC
	binary.LittleEndian.PutUint16(b[18:], machine) // e_machine
	binary.LittleEndian.PutUint32(b[20:], 1)       // e_version
	binary.LittleEndian.PutUint16(b[52:], 64)      // e_ehsize
	return b
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "binary.download")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyELFAcceptsMatchingArch(t *testing.T) {
	for goarch, machine := range map[string]uint16{"amd64": 62, "arm64": 183} {
		path := writeTemp(t, minimalELF(machine))
		if err := VerifyELF(path, goarch); err != nil {
			t.Fatalf("%s: %v", goarch, err)
		}
	}
}

func TestVerifyELFRejectsWrongArch(t *testing.T) {
	path := writeTemp(t, minimalELF(62)) // x86-64 binary
	if err := VerifyELF(path, "arm64"); err == nil {
		t.Fatal("x86-64 binary accepted for arm64")
	}
}

// An HTML error page or truncated download is exactly what the check must
// keep from landing on the real path.
func TestVerifyELFRejectsNonELF(t *testing.T) {
	path := writeTemp(t, []byte("<html>404 Not Found</html>"))
	if err := VerifyELF(path, "amd64"); err == nil {
		t.Fatal("non-ELF file accepted")
	}
}

func TestVerifyELFRejectsUnknownArch(t *testing.T) {
	path := writeTemp(t, minimalELF(62))
	if err := VerifyELF(path, "riscv64"); err == nil {
		t.Fatal("unsupported arch accepted")
	}
}
