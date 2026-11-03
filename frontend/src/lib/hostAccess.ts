// Which host paths an app may bind — a mirror of internal/apps/hostaccess.go,
// so the form can ask for the acknowledgement before the server refuses
// the save. The server decides; this only explains early.
//
// fog's own storage (a Drive folder, a NAS share, the app's own data) binds
// freely. Any other host path needs the operator's acknowledgement. A short
// list is never bound at all.

const DEFAULT_DATA_DIR = "/var/lib/fog";

const REFUSED: [string, string][] = [
  ["/proc", "the host's process table"],
  ["/sys", "the host's kernel and device controls"],
  ["/dev", "the host's devices, disks included"],
  ["/var/run/docker.sock", "Docker's control socket, which is root on this machine"],
  ["/run/docker.sock", "Docker's control socket, which is root on this machine"],
  ["/run/docker", "Docker's runtime state, which includes its control socket"],
  ["/var/run/docker", "Docker's runtime state, which includes its control socket"],
  ["/var/lib/docker", "Docker's own storage — every container's files"],
  ["/run/containerd", "containerd's control socket, which is root on this machine"],
  ["/var/run/containerd", "containerd's control socket, which is root on this machine"],
  ["/var/lib/containerd", "containerd's own storage — every container's files"],
  ["/run/podman", "Podman's control socket, which is root on this machine"],
  ["/var/run/podman", "Podman's control socket, which is root on this machine"],
  ["/run/crio", "CRI-O's control socket, which is root on this machine"],
];
const SOCKETS = ["docker.sock", "containerd.sock", "podman.sock", "crio.sock", "dockershim.sock"];
// The host's time zone and the zone database /etc/localtime links into:
// read-only, a folder covering what is inside it.
const READ_ONLY_EXEMPT = ["/etc/localtime", "/etc/timezone", "/usr/share/zoneinfo"];

// cleanHostPath is filepath.Clean of an absolute path: "//", trailing
// slashes, "." and ".." collapsed.
export function cleanHostPath(p: string): string {
  const out: string[] = [];
  for (const seg of p.trim().split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") out.pop();
    else out.push(seg);
  }
  return "/" + out.join("/");
}

const within = (p: string, base: string) => p === base || base === "/" || p.startsWith(base + "/");

export interface HostStorage {
  shares: string[]; // NAS share mount points
  filesRoot?: string; // Drive's root, when it isn't the default
  app?: string;
}

// hostAccessVerdict judges a bind of raw: refused (never bound), or the
// acknowledgement it needs, or neither for fog's own storage.
export function hostAccessVerdict(raw: string, readOnly: boolean, h: HostStorage): { refused: string; needsAck: string } {
  const none = { refused: "", needsAck: "" };
  if (!raw.trim()) return none;
  const p = cleanHostPath(raw);
  if (p === "/") return { refused: "the host's whole filesystem", needsAck: "" };
  for (const [s, why] of REFUSED) {
    if (within(p, s) || within(s, p)) return { refused: why, needsAck: "" };
  }
  if (SOCKETS.includes(p.slice(p.lastIndexOf("/") + 1))) {
    return { refused: "a container runtime's control socket, which is root on this machine", needsAck: "" };
  }
  const drives = [DEFAULT_DATA_DIR + "/files", ...(h.filesRoot ? [cleanHostPath(h.filesRoot)] : [])];
  const inDrive = drives.some((d) => within(p, d) && !within(p, d + "/.fog") && !/^\/(Apps|Content)\/\.fog(\/|$)/i.test(p.slice(d.length)));
  if (!inDrive) {
    const own = h.app ? within(p, `${DEFAULT_DATA_DIR}/data/${h.app}`) : false;
    if (!own && (within(p, DEFAULT_DATA_DIR) || within(DEFAULT_DATA_DIR, p))) {
      return { refused: `fog's own data directory (${DEFAULT_DATA_DIR}), which holds every app's secrets and the vault key`, needsAck: "" };
    }
  }
  if (readOnly && READ_ONLY_EXEMPT.some((e) => within(p, e))) return none;
  const drive = drives.find((d) => within(p, d));
  if (inDrive && drive !== undefined) return { refused: "", needsAck: driveVerdict(p.slice(drive.length), h.app ?? "") };
  if (h.shares.some((m) => m && m !== "/" && within(p, cleanHostPath(m)))) return none;
  if (h.app && within(p, `${DEFAULT_DATA_DIR}/data/${h.app}`)) return none;
  return { refused: "", needsAck: "a folder on this server outside fog's own storage (Drive, NAS shares, the app's own data)" };
}

// cleanDriveRel is a Drive-relative path cleaned: no leading or trailing
// slash, "." and ".." collapsed.
export const cleanDriveRel = (rel: string) => cleanHostPath(rel).slice(1);

// ackKey is an acknowledgement's spelling for comparison: a host path
// cleaned, a Drive folder cleaned as Drive-relative (it has no leading
// slash).
export const ackKey = (a: string) => (a.trim().startsWith("/") ? cleanHostPath(a) : cleanDriveRel(a));

// driveVerdict mirrors the server's driveWhy: why binding the Drive folder
// rel needs an acknowledgement, or "" when it is the app's to bind — its
// own Apps/<App>, a library, an ordinary folder. A folder another app
// shares with this one is the server's to judge (it knows the shares).
export function driveVerdict(rel: string, app: string): string {
  const r = cleanDriveRel(rel);
  const segs = r.split("/");
  const eq = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();
  if (r === "") return "Drive's whole root — every app's data and every library at once";
  if (eq(segs[0], "Peers")) return "the folders paired households share with this one";
  if (!eq(segs[0], "Apps")) return "";
  if (segs.length === 1) return "every app's own data folder at once — their files, credentials included";
  if (app && eq(segs[1], app)) return "";
  return `${segs[1]}'s own data folder — that app's files, credentials included`;
}
