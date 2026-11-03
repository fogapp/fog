package setup

import (
	"os"
	"path/filepath"
)

// writeFileAbs creates parent directories before writing: on a freshly
// extracted tree, the file's directory existing is exactly the kind of
// assumption this engine must not make.
func writeFileAbs(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
