// The Files tools: what the operator has put in fog's own root, readable
// by an assistant. List, search, and read text — bounded, because every
// byte lands in a context window, and read-only, because a misread
// instruction must not be able to delete a photo library.
//
// Not all of Drive, by default. Apps/ is every app's own data — databases,
// session stores, the credentials an app keeps for the services it talks
// to — and dotfiles are where tools keep theirs (.env, .ssh, .netrc). A
// token reaches either only when the operator minted it with "may read app
// data and hidden files"; otherwise both are refused as if absent, by name
// and through any symlink that would lead into them.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitea.lan/matthew/fog/internal/files"
)

var errNoFiles = errors.New("files is not available")

// filesReadMax bounds one read.
const filesReadMax = 256 * 1024

func (s *server) addFilesTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_files_list",
		Description: "List one folder of Drive (fog's own file tree): name, kind, size, modified time. Paths are relative to the Drive root; empty is the root. Up to 200 entries."},
		s.filesList)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_files_search",
		Description: "Search Drive by name (substring, case-insensitive), optionally under one folder. Returns paths."},
		s.filesSearch)
	mcp.AddTool(srv, &mcp.Tool{Name: "fog_files_read",
		Description: "Read a text file from Drive (Markdown, plain text, JSON, YAML, CSV, code). Binary files are refused; long files are truncated at 256 KiB and say so."},
		s.filesRead)
}

type filesPathIn struct {
	Path string `json:"path,omitempty" jsonschema:"folder path relative to the Drive root; empty for the root"`
}

type filesEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	// Display is the name with control and bidi characters made visible
	// (files.DisplayName), present only when it differs: what to show a
	// person. Name and Path stay exact, for passing back.
	Display string `json:"display,omitempty"`
}

type filesListOut struct {
	Path    string       `json:"path"`
	Total   int          `json:"total"`
	Entries []filesEntry `json:"entries"`
}

func toEntries(nodes []files.Node) []filesEntry {
	out := make([]filesEntry, 0, len(nodes))
	for _, n := range nodes {
		e := filesEntry{Name: n.Name, Path: n.Path, Kind: n.Kind, Size: n.Size, Modified: n.Modified}
		if d := files.DisplayName(n.Path); d != n.Path {
			e.Display = d
		}
		out = append(out, e)
	}
	return out
}

// errOffLimits is what a narrow token hears for a path in Apps/ or a
// dotfile. It names the right that would reach it, so an assistant can tell
// its operator what to change rather than guessing the file is missing.
var errOffLimits = errors.New("that path is outside this token's reach: app data (Apps/) and hidden files need a token created with \"may read app data and hidden files\"")

// offLimits reports whether a cleaned Drive path is out of a narrow token's
// reach: anything under Apps, and any path with a dot-named component.
// Apps is matched case-insensitively, ignoring trailing dots and spaces —
// the root may sit on a case-insensitive or SMB-backed filesystem where
// "apps/", "APPS" or "Apps." open the same folder.
func offLimits(rel string) bool {
	if rel == "" {
		return false
	}
	segs := strings.Split(rel, "/")
	if strings.EqualFold(strings.TrimRight(segs[0], ". "), files.AppsDir) {
		return true
	}
	for _, seg := range segs {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// driveReach cleans a client path and, for a narrow token, refuses one that
// names somewhere off limits.
func driveReach(ctx context.Context, p string) (rel string, all bool, err error) {
	rel, err = files.CleanRel(p)
	if err != nil {
		return "", false, err
	}
	all = canReadAllFiles(ctx)
	if !all && offLimits(rel) {
		return "", false, errOffLimits
	}
	return rel, all, nil
}

// checkNoSymlinks walks rel from the Drive root one component at a time and
// refuses a symlink anywhere along it. A name check alone is not enough: a
// folder link "Notes/x -> ../Apps" — which an app with a Drive volume can
// plant — makes "Notes/x/Paperless/secret" an Apps path under another name.
//
// The walk holds a handle per directory, and each handle is checked to be
// the very entry its lstat described, so a component swapped for a link
// mid-walk is caught rather than followed. want, when given, is the file a
// caller already opened: the walk must end on that same file, which closes
// the gap between the open and this check.
func (s *server) checkNoSymlinks(rel string, want os.FileInfo) error {
	if rel == "" {
		return nil
	}
	dir, err := os.OpenRoot(s.Files.Root())
	if err != nil {
		return err
	}
	defer func() { dir.Close() }()
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		fi, err := dir.Lstat(seg)
		if err != nil {
			return files.ErrNotFound
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errOffLimits
		}
		if i == len(segs)-1 {
			if want != nil && !os.SameFile(fi, want) {
				return errOffLimits
			}
			return nil
		}
		if !fi.IsDir() {
			return files.ErrNotDirectory
		}
		sub, err := dir.OpenRoot(seg)
		if err != nil {
			return files.ErrNotFound
		}
		here, err := sub.Stat(".")
		if err != nil || !os.SameFile(fi, here) {
			sub.Close()
			return errOffLimits
		}
		dir.Close()
		dir = sub
	}
	return nil
}

// visibleEntries drops, for a narrow token, what offLimits refuses — the
// Apps folder itself at the root, and dotfiles the Drive settings may be
// showing.
func visibleEntries(nodes []files.Node, all bool) []files.Node {
	if all {
		return nodes
	}
	out := nodes[:0:0]
	for _, n := range nodes {
		if !offLimits(n.Path) {
			out = append(out, n)
		}
	}
	return out
}

func (s *server) filesList(ctx context.Context, _ *mcp.CallToolRequest, in filesPathIn) (*mcp.CallToolResult, filesListOut, error) {
	if s.Files == nil {
		return nil, filesListOut{}, errNoFiles
	}
	rel, all, err := driveReach(ctx, in.Path)
	if err != nil {
		return nil, filesListOut{}, err
	}
	if !all {
		if err := s.checkNoSymlinks(rel, nil); err != nil {
			return nil, filesListOut{}, err
		}
	}
	l, err := s.Files.List(rel, files.ListOpts{Limit: 200})
	if err != nil {
		return nil, filesListOut{}, err
	}
	entries := visibleEntries(l.Entries, all)
	total := l.Total - (len(l.Entries) - len(entries))
	return nil, filesListOut{Path: rel, Total: total, Entries: toEntries(entries)}, nil
}

type filesSearchIn struct {
	Query string `json:"query" jsonschema:"text to find in file and folder names"`
	Under string `json:"under,omitempty" jsonschema:"restrict to this folder (relative path)"`
}

type filesSearchOut struct {
	Entries []filesEntry `json:"entries"`
}

// filesSearchMax is how many hits one search returns.
const filesSearchMax = 50

func (s *server) filesSearch(ctx context.Context, _ *mcp.CallToolRequest, in filesSearchIn) (*mcp.CallToolResult, filesSearchOut, error) {
	if s.Files == nil {
		return nil, filesSearchOut{}, errNoFiles
	}
	under, all, err := driveReach(ctx, in.Under)
	if err != nil {
		return nil, filesSearchOut{}, err
	}
	if !all {
		if err := s.checkNoSymlinks(under, nil); err != nil {
			return nil, filesSearchOut{}, err
		}
	}
	// A narrow token asks for more than it returns: hits in Apps/ are
	// dropped after the query, and should not crowd out the rest.
	limit := filesSearchMax
	if !all {
		limit *= 4
	}
	hits, err := s.Files.Search(in.Query, under, "", limit)
	if err != nil {
		return nil, filesSearchOut{}, err
	}
	hits = visibleEntries(hits, all)
	if len(hits) > filesSearchMax {
		hits = hits[:filesSearchMax]
	}
	return nil, filesSearchOut{Entries: toEntries(hits)}, nil
}

type filesReadIn struct {
	Path string `json:"path" jsonschema:"file path relative to the Drive root"`
}

type filesReadOut struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Mime      string `json:"mime"`
	Truncated bool   `json:"truncated,omitempty"`
	Text      string `json:"text"`
}

func textLike(mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/x-yaml", "application/toml":
		return true
	}
	return false
}

func (s *server) filesRead(ctx context.Context, _ *mcp.CallToolRequest, in filesReadIn) (*mcp.CallToolResult, filesReadOut, error) {
	if s.Files == nil {
		return nil, filesReadOut{}, errNoFiles
	}
	rel, all, err := driveReach(ctx, in.Path)
	if err != nil {
		return nil, filesReadOut{}, err
	}
	f, n, err := s.Files.OpenFile(rel)
	if err != nil {
		return nil, filesReadOut{}, err
	}
	defer f.Close()
	if !all {
		// OpenFile follows a folder link inside the root; the walk proves
		// the file it opened is the one rel names without one.
		opened, err := f.Stat()
		if err != nil {
			return nil, filesReadOut{}, err
		}
		if err := s.checkNoSymlinks(rel, opened); err != nil {
			return nil, filesReadOut{}, err
		}
	}
	if !textLike(n.Mime) {
		return nil, filesReadOut{}, fmt.Errorf("%s is %s, not text", rel, n.Mime)
	}
	buf, err := io.ReadAll(io.LimitReader(f, filesReadMax+1))
	if err != nil {
		return nil, filesReadOut{}, err
	}
	out := filesReadOut{Path: rel, Size: n.Size, Mime: n.Mime}
	if len(buf) > filesReadMax {
		buf = buf[:filesReadMax]
		out.Truncated = true
	}
	if !utf8.Valid(buf) {
		return nil, filesReadOut{}, fmt.Errorf("%s is not valid UTF-8 text", rel)
	}
	out.Text = string(buf)
	return nil, out, nil
}
