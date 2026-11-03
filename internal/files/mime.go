package files

import (
	"io/fs"
	"mime"
	"path"
	"strings"
)

// Go's built-in table is tiny and the system's /etc/mime.types is not
// guaranteed to exist (the installer's image is minimal). These are the
// types a home server actually meets; anything else falls through to the
// system table and then to application/octet-stream.
var mimeByExt = map[string]string{
	".md": "text/markdown", ".markdown": "text/markdown", ".txt": "text/plain", ".log": "text/plain",
	".csv": "text/csv", ".tsv": "text/tab-separated-values", ".yaml": "text/yaml", ".yml": "text/yaml",
	".toml": "text/plain", ".ini": "text/plain", ".conf": "text/plain", ".env": "text/plain",
	".json": "application/json", ".xml": "application/xml", ".html": "text/html", ".htm": "text/html",
	".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript", ".ts": "text/typescript",
	".tsx": "text/typescript", ".jsx": "text/javascript", ".go": "text/x-go", ".rb": "text/x-ruby",
	".py": "text/x-python", ".sh": "text/x-shellscript", ".rs": "text/x-rust", ".sql": "text/x-sql",
	".c": "text/x-c", ".h": "text/x-c", ".cpp": "text/x-c++", ".java": "text/x-java", ".swift": "text/x-swift",
	".pdf": "application/pdf", ".epub": "application/epub+zip",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
	".avif": "image/avif", ".heic": "image/heic", ".heif": "image/heif", ".svg": "image/svg+xml", ".bmp": "image/bmp",
	".tif": "image/tiff", ".tiff": "image/tiff", ".ico": "image/x-icon", ".raw": "image/x-raw", ".dng": "image/x-adobe-dng",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime", ".mkv": "video/x-matroska",
	".webm": "video/webm", ".avi": "video/x-msvideo", ".wmv": "video/x-ms-wmv", ".ts.mp4": "video/mp2t",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".flac": "audio/flac", ".wav": "audio/wav",
	".ogg": "audio/ogg", ".opus": "audio/opus", ".aiff": "audio/aiff",
	".zip": "application/zip", ".tar": "application/x-tar", ".gz": "application/gzip", ".tgz": "application/gzip",
	".bz2": "application/x-bzip2", ".xz": "application/x-xz", ".zst": "application/zstd", ".7z": "application/x-7z-compressed",
	".rar": "application/vnd.rar", ".dmg": "application/x-apple-diskimage", ".iso": "application/x-iso9660-image",
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt": "application/vnd.oasis.opendocument.text", ".ods": "application/vnd.oasis.opendocument.spreadsheet",
	".key": "application/x-iwork-keynote-sffkey", ".pages": "application/x-iwork-pages-sffpages", ".numbers": "application/x-iwork-numbers-sffnumbers",
	".enc": "application/octet-stream", ".dump": "application/octet-stream", ".db": "application/vnd.sqlite3", ".sqlite": "application/vnd.sqlite3",
	".ttf": "font/ttf", ".otf": "font/otf", ".woff": "font/woff", ".woff2": "font/woff2",
}

// MimeFor guesses a type from the name alone; the index never reads file
// bytes for this. Unknown extensions are octet-stream, which the UI treats
// as "download only".
func MimeFor(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return "application/octet-stream"
	}
	if t, ok := mimeByExt[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		// Strip parameters (Go adds "; charset=utf-8" to text types).
		if i := strings.IndexByte(t, ';'); i >= 0 {
			t = strings.TrimSpace(t[:i])
		}
		return t
	}
	return "application/octet-stream"
}

// storeInZip reports whether a type is already compressed and should be
// stored, not deflated, in an archive — deflating a JPEG costs CPU for
// nothing.
func storeInZip(mimeType string) bool {
	switch {
	case strings.HasPrefix(mimeType, "image/") && mimeType != "image/svg+xml" && mimeType != "image/bmp":
		return true
	case strings.HasPrefix(mimeType, "video/"), strings.HasPrefix(mimeType, "audio/"):
		return true
	}
	switch mimeType {
	case "application/zip", "application/gzip", "application/x-bzip2", "application/x-xz", "application/zstd",
		"application/x-7z-compressed", "application/vnd.rar", "application/pdf", "application/epub+zip",
		"font/woff", "font/woff2", "application/x-apple-diskimage",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return true
	}
	return false
}

func modeString(kind string, bits uint32) string {
	m := fs.FileMode(bits)
	switch kind {
	case "dir":
		m |= fs.ModeDir
	case "other":
		if m&fs.ModeType == 0 {
			m |= fs.ModeSymlink
		}
	}
	return m.String()
}
