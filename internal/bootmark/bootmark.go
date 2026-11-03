// Package bootmark tells a clean fogd stop apart from a crash or power cut.
// The last act of every deliberate exit is writing a marker file; the first
// act of every boot is removing it. Booting without the marker (when state
// already existed) means the previous stop wasn't graceful — that boot owes
// the operator an integrity pass and an honest notice, not a silent restart.
package bootmark

import (
	"os"
	"path/filepath"
	"time"
)

const fileName = "clean-shutdown"

// BeginBoot consumes the marker and reports whether this boot follows an
// unclean stop. A true first boot (no database yet) is never dirty — there
// was nothing to have corrupted.
func BeginBoot(dataDir, dbPath string) (dirty bool) {
	marker := filepath.Join(dataDir, fileName)
	_, merr := os.Stat(marker)
	_, derr := os.Stat(dbPath)
	os.Remove(marker)
	return merr != nil && derr == nil
}

// EndClean records a graceful stop. Call it on every deliberate exit path —
// signal shutdown, self-update restart, recovery-kit restart — and nowhere
// else.
func EndClean(dataDir string) {
	os.WriteFile(filepath.Join(dataDir, fileName),
		[]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}
