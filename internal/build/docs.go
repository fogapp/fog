package build

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A repo's root-level markdown is the app describing itself in prose:
// README, CHANGELOG, a getting-started guide. Fog shows those on the app
// page beside the fog.yaml notes, read straight from the checkout the last
// deploy left behind — never fetched on demand, so what the page says is
// what was built.

// RepoDoc is one markdown file in the repo root.
type RepoDoc struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

// RepoDocContent is one doc's text. Truncated says the file was longer than
// fog is willing to hand a page.
type RepoDocContent struct {
	File      string `json:"file"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// maxDocBytes caps what one doc request returns: a changelog can run to
// megabytes, and a page that renders that much markdown is a page nobody
// wanted.
const maxDocBytes = 512 * 1024

// ErrNoDoc is returned for a file that isn't a root-level markdown file of
// the repo — missing, not markdown, or not a plain file.
var ErrNoDoc = errors.New("no such doc")

// RepoDocs lists the markdown files in the app's checked-out repo root. An
// app with no checkout (image apps, git apps that never fetched) has none.
func (b *Builder) RepoDocs(app string) []RepoDoc {
	return listDocs(filepath.Join(b.appDir(app), "repo"))
}

// ReadRepoDoc returns one root-level markdown file of the app's checkout.
func (b *Builder) ReadRepoDoc(app, file string) (RepoDocContent, error) {
	return readDoc(filepath.Join(b.appDir(app), "repo"), file)
}

// isDocName accepts a bare filename with a markdown extension. Anything
// with a path separator is refused before the filesystem is consulted, so
// a request can only ever name a file in the repo root.
func isDocName(name string) bool {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

func listDocs(dir string) []RepoDoc {
	out := []RepoDoc{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		// Symlinks are skipped, not followed: a repo may point one at
		// anything on the host, and fogd must not read it on the repo's say-so.
		if !e.Type().IsRegular() || !isDocName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, RepoDoc{File: e.Name(), Size: info.Size()})
	}
	// The README leads; the rest read alphabetically, case aside.
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := isReadme(out[i].File), isReadme(out[j].File)
		if ri != rj {
			return ri
		}
		return strings.ToLower(out[i].File) < strings.ToLower(out[j].File)
	})
	return out
}

func isReadme(name string) bool {
	return strings.EqualFold(strings.TrimSuffix(name, filepath.Ext(name)), "readme")
}

func readDoc(dir, file string) (RepoDocContent, error) {
	if !isDocName(file) {
		return RepoDocContent{}, ErrNoDoc
	}
	p := filepath.Join(dir, file)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return RepoDocContent{}, ErrNoDoc
	}
	f, err := os.Open(p)
	if err != nil {
		return RepoDocContent{}, ErrNoDoc
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, maxDocBytes+1))
	if err != nil {
		return RepoDocContent{}, err
	}
	doc := RepoDocContent{File: file}
	if len(buf) > maxDocBytes {
		doc.Content = string(buf[:maxDocBytes])
		doc.Truncated = true
	} else {
		doc.Content = string(buf)
	}
	return doc, nil
}
