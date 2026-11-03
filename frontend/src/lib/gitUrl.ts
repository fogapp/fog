// Mirrors the backend's validateGitRepo for instant inline feedback; the
// server remains authoritative.
export function validateGitRepo(repo: string): string | null {
  const r = repo.trim();
  if (!r) return null; // emptiness handled by required-field logic
  if (/\s/.test(r)) return "The URL contains whitespace — check for a stray space.";
  if (r.startsWith("http://") || r.startsWith("https://"))
    return "That's an HTTP clone URL — deploy keys work over SSH only. Use e.g. ssh://gitea@host/owner/repo.git";
  if (r.startsWith("git://"))
    return "git:// URLs are unauthenticated and unsupported — use the SSH form.";
  if (r.startsWith("ssh://")) {
    const m = r.match(/^ssh:\/\/(?:([^@/]+)@)?([^/]+)(\/.*)?$/);
    if (!m) return "Not a valid ssh:// URL — expected ssh://user@host/owner/repo.git";
    if (!m[1]) return `Missing the SSH user — most git hosts need one, e.g. ssh://gitea@${m[2]}${m[3] ?? ""}`;
    if (!m[3] || m[3].replace(/\//g, "") === "")
      return "Missing the repository path — e.g. ssh://user@host/owner/repo.git";
    return null;
  }
  if (r.startsWith("/")) return null; // local path on the server
  if (/^[A-Za-z0-9._-]+@[A-Za-z0-9._\-[\]]+:[^\s]+$/.test(r)) return null; // scp-style
  return "Unrecognized URL — use ssh://user@host/owner/repo.git (or user@host:owner/repo.git)";
}
