package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // registers the GIF decoder with image.Decode
	"image/jpeg"
	_ "image/png" // registers the PNG decoder
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

	"gitea.lan/matthew/fog/internal/execx"
	"gitea.lan/matthew/fog/internal/fsx"
	"gitea.lan/matthew/fog/internal/hostpkg"
)

// Thumbnails are made on demand and cached under <DataDir>/files-thumbs
// keyed by the file's content digest, so a renamed or moved file never
// renders twice and a rewritten one always renders afresh. JPEG, PNG, GIF
// and WebP decode in-process (stdlib plus x/image); video and HEIC go
// through ffmpeg, which Loop installs on a Linux host that lacks it (and
// which are simply an icon until it is there). Nothing here reads a file
// for its hash alone: an image decoded for a thumbnail is hashed in the
// same read, and the index learns the digest as a side effect; a video
// the hasher hasn't reached yet is cached by inode, size and mtime until
// it has (see renderThumb).

// thumbWidths are the sizes the page asks for; anything else snaps to
// the nearest, so the cache holds a bounded set per file.
var thumbWidths = []int{96, 320, 1024}

// thumbMaxSource bounds what is decoded in-process: a 200-megapixel TIFF
// scan would take a gigabyte to decode, and a home server has apps to run.
const thumbMaxSource = 64 << 20

// The file size says little about what decoding costs: a 400 KB PNG can
// declare 20000×20000 pixels and want 1.6 GB, and an app can put one in a
// folder the operator will browse. So the header is read first and the
// image refused, before a byte of pixels is allocated, when it has more
// than thumbMaxPixels or its decoded form would need more than
// thumbDecodeBudget. The same budget bounds all in-process decoding at
// once (thumbMem), and thumbFFmpeg bounds the ffmpeg runs, which get the
// same pixel limit (-max_pixels).
const (
	thumbMaxPixels    = 64_000_000 // every phone and most cameras; a gigapixel pano is an icon
	thumbDecodeBudget = 384 << 20
)

var (
	thumbMem    = semaphore.NewWeighted(thumbDecodeBudget)
	thumbFFmpeg = semaphore.NewWeighted(2)
)

// thumbWait is how long a render waits its turn before giving up (the page
// shows an icon and asks again later).
const thumbWait = 30 * time.Second

// decodeCost estimates the bytes image.Decode allocates for an image of
// this header: the pixel buffer in the decoder's own model, and for JPEG
// the coefficients a progressive file keeps besides.
func decodeCost(cfg image.Config, format string) (int64, error) {
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, errors.New("empty image")
	}
	px := int64(cfg.Width) * int64(cfg.Height)
	if px > thumbMaxPixels {
		return 0, fmt.Errorf("%dx%d is more than %d megapixels", cfg.Width, cfg.Height, thumbMaxPixels/1_000_000)
	}
	var bpp int64
	switch cfg.ColorModel {
	case color.GrayModel, color.AlphaModel:
		bpp = 1
	case color.Gray16Model, color.Alpha16Model:
		bpp = 2
	case color.RGBAModel, color.NRGBAModel, color.CMYKModel, color.NYCbCrAModel:
		bpp = 4
	case color.YCbCrModel:
		bpp = 3
	default:
		if _, ok := cfg.ColorModel.(color.Palette); ok {
			bpp = 1
		} else {
			bpp = 8 // RGBA64, NRGBA64, or a model this table does not know
		}
	}
	if format == "jpeg" {
		bpp = 8 // a progressive JPEG holds every coefficient until the end
	}
	cost := px * bpp
	if cost > thumbDecodeBudget {
		return 0, fmt.Errorf("%dx%d would take %d MB to decode", cfg.Width, cfg.Height, cost>>20)
	}
	// The thumbnail itself and the decoder's working set, roughly.
	return cost + 4<<20, nil
}

var errNoThumb = errors.New("no thumbnail for this file")

func snapWidth(w int) int {
	best := thumbWidths[0]
	for _, c := range thumbWidths {
		if abs(c-w) < abs(best-w) {
			best = c
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (m *Manager) thumbsDir() string { return filepath.Join(m.DataDir, "files-thumbs") }

// validDigest is a lowercase hex sha256: the only thing that may name a
// file in the thumbnail cache.
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func thumbPath(dir, sha string, w int) string {
	return filepath.Join(dir, sha[:2], fmt.Sprintf("%s-%d.jpg", sha, w))
}

// thumbKind says how a type is rendered: "image" in-process, "ffmpeg" via
// the host tool, "" not at all.
func thumbKind(mime string) string {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return "image"
	case "image/heic", "image/heif", "image/tiff", "image/bmp", "image/avif":
		return "ffmpeg"
	}
	if strings.HasPrefix(mime, "video/") {
		return "ffmpeg"
	}
	return ""
}

// ffmpeg is looked up on demand. A hit is kept for good; a miss is asked
// again after a little while, so the install below (or an operator's own
// apt-get) takes effect without a restart.
var (
	ffmpegMu      sync.Mutex
	ffmpegPath    string
	ffmpegChecked time.Time
)

const ffmpegRecheck = 30 * time.Second

func haveFFmpeg() string {
	ffmpegMu.Lock()
	defer ffmpegMu.Unlock()
	if ffmpegPath == "" && time.Since(ffmpegChecked) > ffmpegRecheck {
		ffmpegPath, _ = exec.LookPath("ffmpeg")
		ffmpegChecked = time.Now()
	}
	return ffmpegPath
}

// installFFmpeg is the host install, a var so tests never reach apt.
var installFFmpeg = func(ctx context.Context) error {
	return hostpkg.Install(ctx, hostpkg.Run, "ffmpeg")
}

// ensureFFmpeg puts ffmpeg on a Linux host that lacks it, so a video or a
// phone's HEIC gets a thumbnail like any JPEG rather than an icon. Same
// philosophy as Docker and the NAS mount helpers: the boring host setup is
// fog's to do. A failure is logged and left there — the page still works,
// with icons — and the next boot tries again.
func (m *Manager) ensureFFmpeg(ctx context.Context) {
	if !m.InstallTools || runtime.GOOS != "linux" || haveFFmpeg() != "" {
		return
	}
	m.Log.Info("files: installing ffmpeg for video and HEIC thumbnails")
	err := installFFmpeg(ctx)
	switch {
	case err == nil:
		ffmpegMu.Lock()
		ffmpegChecked = time.Time{}
		ffmpegMu.Unlock()
		if haveFFmpeg() == "" {
			m.Log.Warn("files: ffmpeg installed but not on PATH; video thumbnails stay icons")
			return
		}
		m.Log.Info("files: ffmpeg installed")
	case errors.Is(err, hostpkg.ErrNoApt):
		m.Log.Info("files: no ffmpeg and no apt-get; video thumbnails stay icons until it is installed by hand")
	case errors.Is(err, hostpkg.ErrOffline):
		m.Log.Warn("files: ffmpeg install waits for internet access; video thumbnails stay icons until the next start")
	default:
		m.Log.Warn("files: ffmpeg install failed; video thumbnails stay icons", "err", err)
	}
}

// Thumbnail returns a reader for the JPEG thumbnail of rel at (about)
// width w, generating and caching it on first request. The returned
// string is the content digest, usable as an ETag.
func (m *Manager) Thumbnail(ctx context.Context, rel string, w int) (io.ReadSeekCloser, string, time.Time, error) {
	root, ix, err := m.rootAndIndex()
	if err != nil {
		return nil, "", time.Time{}, err
	}
	w = snapWidth(w)
	info, err := lstatNF(root, rel)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, "", time.Time{}, errNoThumb
	}
	_, name := Split(rel)
	kind := thumbKind(MimeFor(name))
	if kind == "" || (kind == "ffmpeg" && haveFFmpeg() == "") {
		return nil, "", time.Time{}, errNoThumb
	}
	if kind == "image" && info.Size() > thumbMaxSource {
		return nil, "", time.Time{}, errNoThumb
	}
	// The digest: from the index when it is current (the row is made to
	// exist first, so a digest learned below can be recorded).
	sha := ""
	row, err := ix.Get(rel)
	if err != nil {
		parent, _ := Split(rel)
		m.refreshDir(root, ix, parent, false)
		row, err = ix.Get(rel)
	}
	if err == nil && row.Size == info.Size() && row.mtime == info.ModTime().UnixNano() && validDigest(row.SHA256) {
		sha = row.SHA256
	}
	if sha != "" {
		if f, err := os.Open(thumbPath(m.thumbsDir(), sha, w)); err == nil {
			return f, sha, info.ModTime(), nil
		}
	}
	// Concurrent requests for one thumbnail (a grid scrolled into view
	// twice) share a single render, which one leaving early mustn't cancel
	// for the rest; ffmpeg carries its own bound.
	key := fmt.Sprintf("%s|%d|%d|%d", rel, w, info.Size(), info.ModTime().UnixNano())
	v, err, _ := thumbFlight.Do(key, func() (any, error) {
		return m.renderThumb(context.WithoutCancel(ctx), root, ix, rel, info, kind, sha, w)
	})
	if err != nil {
		return nil, "", time.Time{}, err
	}
	r := v.(thumbResult)
	f, err := os.Open(r.path)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return f, r.etag, info.ModTime(), nil
}

var thumbFlight singleflight.Group

type thumbResult struct{ path, etag string }

// renderThumb makes and caches one thumbnail. sha is the digest if the
// index knew it. An image not yet hashed is hashed while it decodes, in
// the same read. A video not yet hashed is never read in full just to
// name its thumbnail: it is cached under a key of its inode, size and
// mtime instead (statThumbPath), and moves to its digest once the hasher
// has been through.
func (m *Manager) renderThumb(ctx context.Context, root *os.Root, ix *Index, rel string, info fs.FileInfo, kind, sha string, w int) (thumbResult, error) {
	var out []byte
	var err error
	switch kind {
	case "image":
		var sum string
		out, sum, err = m.renderImage(ctx, root, rel, w)
		if err == nil && sha == "" {
			sha = sum
			ix.SetHash(rel, info.Size(), info.ModTime().UnixNano(), sha)
		}
	case "ffmpeg":
		if sha == "" {
			path, etag := statThumbPath(m.thumbsDir(), info, w)
			if _, err := os.Stat(path); err == nil {
				return thumbResult{path, etag}, nil
			}
			m.kickHasher()
			out, err = m.renderFFmpeg(ctx, root, rel, w)
			if err != nil {
				return thumbResult{}, fmt.Errorf("%w: %v", errNoThumb, err)
			}
			return thumbResult{path, etag}, writeThumb(path, out)
		}
		out, err = m.renderFFmpeg(ctx, root, rel, w)
	}
	if err != nil {
		return thumbResult{}, fmt.Errorf("%w: %v", errNoThumb, err)
	}
	path := thumbPath(m.thumbsDir(), sha, w)
	return thumbResult{path, sha}, writeThumb(path, out)
}

func writeThumb(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, data, 0o644)
}

// statThumbDir holds thumbnails of files whose digest isn't known yet,
// named for the file's inode, size and mtime; the sweep ages them out.
const statThumbDir = "by-stat"

// statThumbMaxAge is how long a by-stat thumbnail is kept: long enough
// for the hasher to reach the file on a big library.
const statThumbMaxAge = 7 * 24 * time.Hour

func statThumbPath(dir string, info fs.FileInfo, w int) (path, etag string) {
	key := fmt.Sprintf("s%x-%x-%x", inodeOf(info), info.Size(), info.ModTime().UnixNano())
	return filepath.Join(dir, statThumbDir, fmt.Sprintf("%s-%d.jpg", key, w)), key
}

// renderImage decodes and scales an image, digesting the file in the same
// read: the decoder sees the bytes through a tee, and whatever it leaves
// unread is drained into the hash. The header is read first (and replayed
// to the decoder), and an image too big to decode safely is refused before
// any pixels are allocated (decodeCost).
func (m *Manager) renderImage(ctx context.Context, root *os.Root, rel string, w int) ([]byte, string, error) {
	f, _, err := openFileNF(root, rel)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	var head bytes.Buffer
	cfg, format, err := image.DecodeConfig(io.TeeReader(f, &head))
	if err != nil {
		return nil, "", err
	}
	cost, err := decodeCost(cfg, format)
	if err != nil {
		return nil, "", err
	}
	wctx, cancel := context.WithTimeout(ctx, thumbWait)
	defer cancel()
	if err := thumbMem.Acquire(wctx, cost); err != nil {
		return nil, "", fmt.Errorf("busy: %w", err)
	}
	defer thumbMem.Release(cost)
	h := sha256.New()
	img, _, err := image.Decode(io.TeeReader(io.MultiReader(&head, f), h))
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(h, f); err != nil {
		return nil, "", err
	}
	out, err := encodeThumb(img, w)
	return out, hex.EncodeToString(h.Sum(nil)), err
}

func encodeThumb(img image.Image, w int) ([]byte, error) {
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return nil, errors.New("empty image")
	}
	tw, th := b.Dx(), b.Dy()
	if tw > w {
		th = th * w / tw
		tw = w
	}
	if th < 1 {
		th = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	// Bilinear over CatmullRom: three times faster and, at thumbnail
	// sizes, indistinguishable.
	draw.BiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ffmpegDemuxers are the containers a thumbnail is made from. Anything
// else is refused before it is parsed: a playlist (HLS, concat) or a
// subtitle format names other files to read, and a file in Drive an app put
// there must not get ffmpeg to open the host's files and paint them into a
// picture fog then serves.
const ffmpegDemuxers = "mov,matroska,avi,mpegts,flv,asf,mpeg,mpegps,ogg,mxf,h264,hevc,m4v,image2," +
	"tiff_pipe,bmp_pipe,jpeg_pipe,png_pipe,webp_pipe,gif,heif"

// renderFFmpeg asks ffmpeg for one frame scaled to w. Video takes the
// frame a few seconds in (the first is often black); stills take frame
// one. Bounded to twenty seconds: a broken file must not pin a worker.
//
// ffmpeg is handed the file already open (fd 3), never its path: the file
// was reached without following a link (nofollow.go), and a path would be
// resolved again by ffmpeg, links and all. It may read only that file
// (-protocol_whitelist file, -format_whitelist) and decode no more pixels
// than an image fog decodes itself (-max_pixels).
func (m *Manager) renderFFmpeg(ctx context.Context, root *os.Root, rel string, w int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := thumbFFmpeg.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("busy: %w", err)
	}
	defer thumbFFmpeg.Release(1)
	f, _, err := openFileNF(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, name := Split(rel)
	video := strings.HasPrefix(MimeFor(name), "video/")
	run := func(seek bool) ([]byte, error) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		args := []string{"-v", "error", "-nostdin",
			"-protocol_whitelist", "file", "-format_whitelist", ffmpegDemuxers,
			"-max_pixels", fmt.Sprint(thumbMaxPixels)}
		if seek {
			args = append(args, "-ss", "3")
		}
		args = append(args, "-i", "file:/dev/fd/3", "-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:-2,format=yuvj420p", w), "-f", "image2", "-c:v", "mjpeg", "-q:v", "4", "pipe:1")
		cmd := execx.Command(ctx, haveFFmpeg(), args...)
		cmd.ExtraFiles = []*os.File{f}
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil || out.Len() == 0 {
			return nil, fmt.Errorf("ffmpeg: %v %s", err, strings.TrimSpace(stderr.String()))
		}
		return out.Bytes(), nil
	}
	out, err := run(video)
	if err != nil && video {
		// A short video: retry at the first frame.
		if out2, err2 := run(false); err2 == nil {
			return out2, nil
		}
	}
	return out, err
}

// Dimensions reads an image's size from its header without decoding it.
func (m *Manager) Dimensions(rel string) (w, h int, ok bool) {
	root, err := m.openRoot()
	if err != nil {
		return 0, 0, false
	}
	_, name := Split(rel)
	if thumbKind(MimeFor(name)) != "image" {
		return 0, 0, false
	}
	f, _, err := openFileNF(root, rel)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// sweepThumbs drops cached thumbnails whose digest no file in the index
// has any more: the file was deleted, or rewritten. Cheap and daily.
func (m *Manager) sweepThumbs() {
	ix, err := m.Index()
	if err != nil {
		return
	}
	dir := m.thumbsDir()
	shards, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, sh := range shards {
		if !sh.IsDir() {
			continue
		}
		if sh.Name() == statThumbDir {
			m.sweepStatThumbs(filepath.Join(dir, sh.Name()))
			continue
		}
		ents, err := os.ReadDir(filepath.Join(dir, sh.Name()))
		if err != nil {
			continue
		}
		for _, e := range ents {
			sha, _, ok := strings.Cut(e.Name(), "-")
			if !ok || len(sha) != 64 {
				continue
			}
			if _, err := hex.DecodeString(sha); err != nil {
				continue
			}
			if n, _ := ix.Duplicates(sha, "\x00", 1); len(n) == 0 {
				os.Remove(filepath.Join(dir, sh.Name(), e.Name()))
			}
		}
	}
}

// sweepStatThumbs drops by-stat thumbnails a week after they were made:
// by then the file has a digest (and renders once more under it), or is
// gone.
func (m *Manager) sweepStatThumbs(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-statThumbMaxAge)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// CachedThumb and StoreThumb expose the cache by content digest for
// thumbnails rendered elsewhere — a peer's origin, for a file this box
// lists but does not hold. Same directory, same sweep: a digest the index
// no longer knows takes its thumbnails with it.
func (m *Manager) CachedThumb(sha string, w int) (io.ReadSeekCloser, error) {
	if !validDigest(sha) {
		return nil, errNoThumb
	}
	return os.Open(thumbPath(m.thumbsDir(), sha, snapWidth(w)))
}

func (m *Manager) StoreThumb(sha string, w int, data []byte) error {
	if !validDigest(sha) {
		return errNoThumb
	}
	return writeThumb(thumbPath(m.thumbsDir(), sha, snapWidth(w)), data)
}

// SnapWidth is the cache's width grid, for callers keying on it.
func SnapWidth(w int) int { return snapWidth(w) }
