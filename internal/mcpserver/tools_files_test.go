package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.lan/matthew/fog/internal/files"
	"gitea.lan/matthew/fog/internal/store"
)

const appSecret = "PAPERLESS_DB_PASSWORD=hunter2"

// newDriveHandler builds the MCP handler over a real Drive holding an app's
// credentials, dotfiles, and the symlinks an app with a Drive volume could
// plant, and answers for the token given.
func newDriveHandler(t *testing.T, tok *store.MCPToken) (http.Handler, *files.Manager) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fm := files.New(dir, st, log)
	root := fm.Root()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Apps/Paperless/creds.txt", appSecret)
	write("Apps/Paperless/notes.md", "paperless notes")
	write(".env", "OPERATOR_TOKEN=s3cret")
	write("Notes/todo.md", "buy milk")
	write("Notes/.ssh/id.txt", "ssh key")
	write("Notes/creds-howto.md", "how to rotate creds")
	// What an app holding Notes as a volume can do: link into another
	// app's data, as a folder and as a file.
	if err := os.Symlink("../Apps", filepath.Join(root, "Notes", "applink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Apps/Paperless/creds.txt", filepath.Join(root, "Notes", "creds.txt")); err != nil {
		t.Fatal(err)
	}
	// Show dotfiles in Drive: the MCP narrowing must not lean on a UI
	// preference the operator can turn on.
	if err := st.SetSetting(files.SettingShowHidden, "on"); err != nil {
		t.Fatal(err)
	}
	if err := fm.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	inner := New(Deps{Store: st, Files: fm, Log: log})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(WithToken(r.Context(), tok)))
	}), fm
}

func resultText(res callResult) string {
	var b strings.Builder
	b.Write(res.Result.StructuredContent)
	for _, c := range res.Result.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// A default token — the kind every assistant gets unless the operator says
// otherwise — tries every way into another app's credentials and the
// operator's dotfiles. None may return a byte of them.
func TestFilesReadDefaultTokenCannotReachAppDataOrDotfiles(t *testing.T) {
	h, _ := newDriveHandler(t, &store.MCPToken{Name: "assistant"})

	// The control: an ordinary note reads fine.
	if res := call(t, h, "fog_files_read", map[string]string{"path": "Notes/todo.md"}); res.Result.IsError || !strings.Contains(resultText(res), "buy milk") {
		t.Fatalf("ordinary read failed: %s", resultText(res))
	}

	for _, p := range []string{
		"Apps/Paperless/creds.txt",
		"/Apps/Paperless/creds.txt",
		"./Apps/Paperless/creds.txt",
		"Notes/../Apps/Paperless/creds.txt",
		"apps/Paperless/creds.txt", // a case-insensitive root opens this
		"APPS/Paperless/creds.txt",
		"Apps./Paperless/creds.txt", // SMB strips the trailing dot
		"Apps%2FPaperless%2Fcreds.txt",
		"Notes/applink/Paperless/creds.txt", // a folder link into Apps
		"Notes/creds.txt",                   // a file link into Apps
		".env",
		"Notes/.ssh/id.txt",
		"Notes/../.env",
	} {
		res := call(t, h, "fog_files_read", map[string]string{"path": p})
		text := resultText(res)
		if !res.Result.IsError || strings.Contains(text, "hunter2") || strings.Contains(text, "s3cret") || strings.Contains(text, "ssh key") {
			t.Errorf("read %q: isError=%v, answer %q — want a refusal with no content", p, res.Result.IsError, text)
		}
	}
}

func TestFilesListAndSearchDefaultTokenHideAppData(t *testing.T) {
	h, _ := newDriveHandler(t, &store.MCPToken{Name: "assistant"})

	res := call(t, h, "fog_files_list", map[string]string{"path": ""})
	if res.Result.IsError {
		t.Fatalf("root list: %s", resultText(res))
	}
	var l filesListOut
	if err := json.Unmarshal(res.Result.StructuredContent, &l); err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Entries {
		if e.Path == "Apps" || strings.HasPrefix(e.Name, ".") {
			t.Errorf("root listing shows %q to a default token", e.Path)
		}
	}
	for _, p := range []string{"Apps", "Apps/Paperless", "apps/Paperless", "Notes/applink", "Notes/applink/Paperless", "Notes/.ssh"} {
		if res := call(t, h, "fog_files_list", map[string]string{"path": p}); !res.Result.IsError {
			t.Errorf("list %q: want a refusal, got %s", p, resultText(res))
		}
	}

	res = call(t, h, "fog_files_search", map[string]string{"query": "creds"})
	if res.Result.IsError {
		t.Fatalf("search: %s", resultText(res))
	}
	text := resultText(res)
	if strings.Contains(text, "Apps/") {
		t.Errorf("search shows app data to a default token: %s", text)
	}
	if !strings.Contains(text, "Notes/creds-howto.md") {
		t.Errorf("search lost an ordinary hit: %s", text)
	}
	if res := call(t, h, "fog_files_search", map[string]string{"query": "id", "under": "Notes/.ssh"}); !res.Result.IsError {
		t.Errorf("search under a dot folder: want a refusal, got %s", resultText(res))
	}
	if res := call(t, h, "fog_files_search", map[string]string{"query": "creds", "under": "Apps"}); !res.Result.IsError {
		t.Errorf("search under Apps: want a refusal, got %s", resultText(res))
	}
}

// The operator's explicit grant is the way in — and the only one.
func TestFilesReadAllFilesTokenReachesAppData(t *testing.T) {
	h, _ := newDriveHandler(t, &store.MCPToken{Name: "trusted", CanReadAllFiles: true})
	if res := call(t, h, "fog_files_read", map[string]string{"path": "Apps/Paperless/creds.txt"}); res.Result.IsError || !strings.Contains(resultText(res), "hunter2") {
		t.Fatalf("broad token read of app data: %s", resultText(res))
	}
	if res := call(t, h, "fog_files_read", map[string]string{"path": ".env"}); res.Result.IsError || !strings.Contains(resultText(res), "s3cret") {
		t.Fatalf("broad token read of a dotfile: %s", resultText(res))
	}
	res := call(t, h, "fog_files_search", map[string]string{"query": "creds"})
	if !strings.Contains(resultText(res), "Apps/Paperless/creds.txt") {
		t.Errorf("broad token search misses app data: %s", resultText(res))
	}
}

// A request that never passed through mcpAuthed carries no grant at all.
func TestFilesUnlabelledContextIsNarrow(t *testing.T) {
	if canReadAllFiles(context.Background()) {
		t.Fatal("an unlabelled context must not read all files")
	}
}

func TestOffLimits(t *testing.T) {
	for p, want := range map[string]bool{
		"":                   false,
		"Notes":              false,
		"Notes/Apps/x":       false, // only the root's Apps is app data
		"Content/Photos/a":   false,
		"Apps":               true,
		"apps/x":             true,
		"Apps /x":            true,
		"Notes/.git/config":  true,
		".bashrc":            true,
		"Notes/a.b/c":        false,
		"Applications/x.txt": false,
	} {
		if got := offLimits(p); got != want {
			t.Errorf("offLimits(%q) = %v, want %v", p, got, want)
		}
	}
}
