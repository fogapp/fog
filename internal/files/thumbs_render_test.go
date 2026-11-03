package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// An image without a digest yet is hashed in the same read that decodes
// it: the thumbnail lands under the digest and the index learns it.
func TestImageThumbnailHashesWhileDecoding(t *testing.T) {
	m := newManager(t)
	m.DataDir = t.TempDir()
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 40, 30)))
	write(t, m, "pics/a.png", buf.String())
	want := sha256.Sum256(buf.Bytes())

	f, etag, _, err := m.Thumbnail(context.Background(), "pics/a.png", 96)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if etag != hex.EncodeToString(want[:]) {
		t.Fatalf("etag %s, want the file's digest", etag)
	}
	if n, err := m.Node("pics/a.png"); err != nil || n.SHA256 != etag {
		t.Fatalf("index digest %q %v", n.SHA256, err)
	}
	if _, err := os.Stat(thumbPath(m.thumbsDir(), etag, 96)); err != nil {
		t.Fatal(err)
	}
}

// A video the hasher hasn't reached is not read in full: its thumbnail is
// cached by stat, and concurrent requests share one ffmpeg run.
func TestVideoThumbnailBeforeDigestRendersOnce(t *testing.T) {
	m := newManager(t)
	m.DataDir = t.TempDir()
	write(t, m, "clips/v.mp4", strings.Repeat("v", 4096))

	bin := t.TempDir()
	count := filepath.Join(bin, "count")
	script := filepath.Join(bin, "ffmpeg")
	os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+count+"\nsleep 0.3\nprintf JPEG\n"), 0o755)
	ffmpegMu.Lock()
	oldPath := ffmpegPath
	ffmpegPath = script
	ffmpegMu.Unlock()
	t.Cleanup(func() {
		ffmpegMu.Lock()
		ffmpegPath = oldPath
		ffmpegMu.Unlock()
	})

	var wg sync.WaitGroup
	etags := make([]string, 4)
	for i := range etags {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, etag, _, err := m.Thumbnail(context.Background(), "clips/v.mp4", 320)
			if err != nil {
				t.Error(err)
				return
			}
			f.Close()
			etags[i] = etag
		}()
	}
	wg.Wait()
	// Once more, after: a cache hit.
	if f, _, _, err := m.Thumbnail(context.Background(), "clips/v.mp4", 320); err == nil {
		f.Close()
	}
	runs, _ := os.ReadFile(count)
	if n := strings.Count(string(runs), "x"); n != 1 {
		t.Fatalf("ffmpeg ran %d times, want 1", n)
	}
	if !strings.HasPrefix(etags[0], "s") {
		t.Fatalf("etag %q: want a by-stat key", etags[0])
	}
	if n, _ := m.Node("clips/v.mp4"); n.SHA256 != "" {
		t.Fatal("the video was hashed synchronously")
	}
}
