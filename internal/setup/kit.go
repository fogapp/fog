package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gitea.lan/matthew/fog/internal/backup"

	_ "modernc.org/sqlite"
)

// KitPayload is a decrypted, verified recovery kit: the two files that are
// a Fog server's identity, plus what we could read out of them to show the
// operator whose server is about to be brought back.
type KitPayload struct {
	DB        []byte
	MasterKey []byte
	// From the kit's own database — so the wizard can say "Restoring
	// server 'fog' (admin: matthew)" instead of asking questions the kit
	// already answers. AdminHash lets a restore verify a typed password
	// against the account it will actually restore.
	ServerName string
	AdminUser  string
	AdminHash  string
}

// Kit files are a master key (bytes) and a vacuumed SQLite db (a few MBs);
// the cap only needs headroom. It matters here more than anywhere: the live
// installer buffers uploads on a tmpfs-rooted system, so an oversized upload
// would eat the RAM the install itself needs. Mirrors the running server's
// restore cap (internal/server/backup_handlers.go).
const maxKitFile = 64 << 20

// ParseKit decrypts and fully verifies a kit before any disk is touched:
// wrong passphrase, corruption, or unexpected contents all fail here, while
// the operator's old server data is still wherever it was.
func ParseKit(sealed []byte, passphrase string) (*KitPayload, error) {
	plain, err := backup.DecryptKit(sealed, passphrase)
	if err != nil {
		return nil, err
	}
	kit := &KitPayload{}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("not a kit archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("kit archive: %w", err)
		}
		// Exactly two known names; anything else in an uploaded archive is
		// refused, never written anywhere.
		switch hdr.Name {
		case "master.key":
			kit.MasterKey, err = readKitFile(tr, hdr.Name)
		case "fog.db":
			kit.DB, err = readKitFile(tr, hdr.Name)
		default:
			return nil, fmt.Errorf("kit contains unexpected file %q", hdr.Name)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(kit.MasterKey) == 0 || len(kit.DB) == 0 {
		return nil, errors.New("kit is missing master.key or fog.db")
	}
	kit.ServerName, kit.AdminUser, kit.AdminHash = peekKitDB(kit.DB)
	return kit, nil
}

// readKitFile reads one kit entry, refusing (rather than silently
// truncating) anything past the cap — a truncated fog.db handed onward
// would fail later, further from the honest explanation.
func readKitFile(r io.Reader, name string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxKitFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxKitFile {
		return nil, fmt.Errorf("kit file %q exceeds the %dMB limit — this is not a Fog recovery kit", name, maxKitFile>>20)
	}
	return b, nil
}

// peekKitDB reads identity out of the kit's database, best-effort — a kit
// whose db can't be read here still restores (fogd migrates it at boot);
// the wizard just has less to say about it.
func peekKitDB(db []byte) (serverName, adminUser, adminHash string) {
	tmp, err := os.CreateTemp("", "fog-kit-peek-*.db")
	if err != nil {
		return "", "", ""
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(db); err != nil {
		tmp.Close()
		return "", "", ""
	}
	tmp.Close()
	conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(tmp.Name())+"?mode=ro")
	if err != nil {
		return "", "", ""
	}
	defer conn.Close()
	conn.QueryRow(`SELECT value FROM settings WHERE key = 'server_name'`).Scan(&serverName)
	// The operator is a person with a password credential. A kit taken
	// before fog had one login instead carries a `users` row, and a restore
	// from one of those has to keep working — so ask the old table when the
	// new one has no answer. Either way the hash is the same argon2id
	// encoding, which is all the caller compares against.
	conn.QueryRow(`SELECT p.handle, c.data FROM people p
		JOIN people_credentials c ON c.person_id = p.id AND c.kind = 'password'
		WHERE p.role = 'operator' ORDER BY p.id LIMIT 1`).Scan(&adminUser, &adminHash)
	if adminUser == "" {
		conn.QueryRow(`SELECT username, password_hash FROM users ORDER BY id LIMIT 1`).Scan(&adminUser, &adminHash)
	}
	return serverName, adminUser, adminHash
}
