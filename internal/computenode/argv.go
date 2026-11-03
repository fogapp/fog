package computenode

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gitea.lan/matthew/fog/internal/compute"
)

// The machine is someone's workstation: an app's command line is checked
// before it runs, and refused rather than trusted
// (docs/compute-contract.md). This is the first line, not the boundary:
// ffmpeg reads hostile input, and has more options than any list, so
// whatever passes here still runs confined (confine.go).

// filterArgs take a filter graph, where a filter could open a file.
var filterArgs = map[string]bool{"-vf": true, "-af": true, "-filter": true, "-filter_complex": true, "-lavfi": true}

// forbiddenOptions read or write files that aren't the run's, or load
// code. An option is matched by its name before any stream specifier
// (-dump_attachment:t is -dump_attachment).
var forbiddenOptions = map[string]bool{
	"-filter_script": true, "-filter_complex_script": true, "-attach": true, "-dump_attachment": true,
	"-passlogfile": true, "-sdp_file": true, "-vstats_file": true, "-report": true,
	"-pre": true, "-fpre": true, "-vpre": true, "-apre": true, "-spre": true,
	"-stats_enc_pre": true, "-stats_enc_post": true, "-stats_mux_pre": true, "-print_graphs_file": true,
	"-dump_attachment_file": true, "-listen": true,
}

// forbiddenFormats are formats that name other files or devices by
// themselves: lavfi sources, tee's outputs, devices.
var forbiddenFormats = map[string]bool{"lavfi": true, "tee": true, "avfoundation": true, "v4l2": true, "x11grab": true,
	"kmsgrab": true, "fbdev": true, "alsa": true, "pulse": true, "oss": true, "jack": true, "openal": true, "decklink": true,
	"dshow": true, "gdigrab": true, "sndio": true, "xcbgrab": true, "lavfi_concat": true}

// fileFilters are filters that open a file, a socket, or a plugin by
// name, and the options that make an otherwise harmless one do so
// (drawtext draws with a system font, but not from a textfile or a
// fontfile).
var fileFilters = regexp.MustCompile(`(?i)(^|[,;\[\]=\s])(a?movie|subtitles|ass|a?sendcmd|a?zmq|lut1d|lut3d|haldclut|frei0r(_src)?|ladspa|lv2|ocr|vidstab\w*|signature|sofalizer|amovie|dnn_\w+|sr|derain|arnndn|libvmaf|vmafmotion|find_rect|removelogo|libplacebo|program_opencl|openclsrc|lensfun|a?metadata|coreimage(src)?)\b|textfile|fontfile|filename|stats_file|log_path|model_path|shader|\bsource=`)

// allowedProtocols are what a run may name in -protocol_whitelist: the
// data proxy, and its own scratch directory.
var allowedProtocols = map[string]bool{"http": true, "https": true, "tcp": true, "tls": true, "file": true}

// protocols are ffmpeg's protocols that reach past a run, by their
// prefix. A run names its data as a {server} URL and its files as paths;
// pipe: (stdin, stdout) is all else it may name.
var protocols = regexp.MustCompile(`(?i)(^|[^a-z0-9_])(async|bluray|cache|concat|concatf|crypto|data|dvd|fd|ftp|gopher|gophers|hls|http|https|httpproxy|icecast|ipfs|ipns|md5|mmsh|mmst|prompeg|rist|rtmp[a-z]*|rtp|sctp|smb|sftp|srt|srtp|subfile|tcp|tee|tls|udp|udplite|unix|zmq|android_content|librtmp\w*|libssh|libsmbclient|libsrt|librist|ffrtmp\w+):`)

// absPath is an absolute path inside an argument: at its start, or after
// a separator in a filter graph, an option list or a protocol.
var absPath = regexp.MustCompile(`(?:^|[=:,|;'"\[\]\s])(/[^,:;|'"\[\]\s]*)`)

// devicePaths may be named for hardware: the GPU's render nodes.
var devicePaths = regexp.MustCompile(`^/dev/dri/renderD[0-9]+$`)

// Prepare checks an argv fog handed over and fills in its placeholders:
// {server} with the run's address on its data proxy, {out} with its
// scratch directory. It answers the command to run, its first element the
// node's own ffmpeg, or why the run is refused.
func Prepare(argv []string, ffmpeg, server, out string) ([]string, error) {
	if len(argv) == 0 || argv[0] != "ffmpeg" {
		return nil, fmt.Errorf("only ffmpeg runs here")
	}
	out = filepath.Clean(out)
	cmd := []string{ffmpeg}
	fill := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "{server}", server), "{out}", out)
	}
	for i := 1; i < len(argv); i++ {
		arg := fill(argv[i])
		prev := optionName(argv[i-1])
		name := optionName(arg)
		switch {
		case strings.HasPrefix(arg, "-/"):
			// ffmpeg 7: -/option reads the option's value from a file.
			return nil, fmt.Errorf("an option read from a file is not allowed: %s", arg)
		case forbiddenOptions[name]:
			return nil, fmt.Errorf("%s is not allowed", arg)
		case filterArgs[prev] && fileFilters.MatchString(arg):
			return nil, fmt.Errorf("a filter that opens files is not allowed: %s", arg)
		case prev == "-protocol_whitelist":
			for _, p := range strings.Split(arg, ",") {
				if !allowedProtocols[p] {
					return nil, fmt.Errorf("protocol %s is not allowed", p)
				}
			}
		case prev == "-i" && !within(arg, server, out):
			return nil, fmt.Errorf("an input outside the run: %s", arg)
		case prev == "-f" && forbiddenFormats[arg]:
			return nil, fmt.Errorf("-f %s is not allowed", arg)
		case prev == "-f" && arg == "concat":
			// A concat list may name only the run's own (its safe mode,
			// left on, refuses absolute paths and protocols in it).
			if !(i+2 < len(argv) && argv[i+1] == "-i" && within(fill(argv[i+2]), server, out)) {
				return nil, fmt.Errorf("-f %s is not allowed", arg)
			}
		case prev == "-safe" && arg != "1":
			return nil, fmt.Errorf("-safe %s is not allowed", arg)
		}
		if err := checkArg(arg, server, out); err != nil {
			return nil, err
		}
		cmd = append(cmd, arg)
	}
	return cmd, nil
}

// optionName is an option without its stream specifier: -c:v is -c. Not
// an option, it's itself.
func optionName(arg string) string {
	if !strings.HasPrefix(arg, "-") || len(arg) < 2 {
		return arg
	}
	name, _, _ := strings.Cut(arg, ":")
	return name
}

// checkArg refuses an argument that names anything outside the run: a URL
// but the run's own data, a protocol that reaches elsewhere, an absolute
// path outside its scratch directory, or a path that climbs.
func checkArg(arg, server, out string) error {
	// The run's own data URLs are allowed; take them out to look at the
	// rest.
	rest := arg
	for {
		i := strings.Index(rest, server+"/")
		if i < 0 {
			break
		}
		end := i + len(server)
		for end < len(rest) && !strings.ContainsRune(" |'\",;[]", rune(rest[end])) {
			end++
		}
		if !within(rest[i:end], server, out) {
			return fmt.Errorf("a URL that climbs out of the run: %s", arg)
		}
		rest = rest[:i] + rest[end:]
	}
	if strings.Contains(rest, server) {
		return fmt.Errorf("a URL outside the run: %s", arg)
	}
	if strings.Contains(rest, "://") {
		return fmt.Errorf("a URL outside the run: %s", arg)
	}
	if protocols.MatchString(rest) {
		return fmt.Errorf("a protocol that reaches outside the run: %s", arg)
	}
	for _, m := range absPath.FindAllStringSubmatch(rest, -1) {
		p := m[1]
		if devicePaths.MatchString(p) {
			continue
		}
		if !within(p, server, out) {
			return fmt.Errorf("a path outside the run: %s", arg)
		}
	}
	if strings.Contains(rest, "..") && climbs(rest) {
		return fmt.Errorf("a path that climbs out of the run: %s", arg)
	}
	return nil
}

// climbs reports a ".." path element anywhere in s.
func climbs(s string) bool {
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune("/\\=:,|;'\" []", r) }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// within reports whether arg names the run's own data (under the app's
// /fog/compute/ on the data proxy) or its scratch directory.
func within(arg, server, out string) bool {
	if strings.HasPrefix(arg, server+compute.AppDataPrefix) {
		rest := strings.TrimPrefix(arg, server)
		return !strings.Contains(rest, "/../") && !strings.HasSuffix(rest, "/..") && !strings.Contains(rest, "\\")
	}
	p := strings.TrimPrefix(arg, "file:")
	if !strings.HasPrefix(p, "/") {
		return false
	}
	p = filepath.Clean(p)
	return p == out || strings.HasPrefix(p, out+string(filepath.Separator))
}
