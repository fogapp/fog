// Mirrors the backend's validateGitRepo for instant inline feedback; the
// server remains authoritative.
export function validateGitRepo(repo: string): string | null {
  const r = repo.trim();
  if (!r) return null; // emptiness handled by required-field logic
  if (/\s/.test(r)) return "The URL contains whitespace — check for a stray space.";
  if (r.startsWith("http://"))
    return "Plain http:// is unencrypted — use https:// for a public repository, or the SSH form (e.g. ssh://gitea@host/owner/repo.git) with a deploy key for a private one.";
  if (r.startsWith("https://")) {
    let u: URL;
    try {
      u = new URL(r);
    } catch {
      return "Not a valid https:// URL — expected https://host/owner/repo.git";
    }
    if (u.username || u.password)
      return "No credentials in the URL — https:// is for public repositories; clone a private one over SSH with the deploy key.";
    if (u.search || u.hash) return "Use the plain clone URL, without a query or fragment.";
    if (!u.pathname.replace(/\//g, "")) return `Missing the repository path — e.g. https://${u.host}/owner/repo.git`;
    return null;
  }
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
  return "Unrecognized URL — use ssh://user@host/owner/repo.git (or user@host:owner/repo.git), or https://host/owner/repo.git for a public repository";
}

// A public repository cloned over https needs no deploy key: there is
// nothing for its host to be given.
export function isHTTPSRepo(repo: string | undefined): boolean {
  return (repo ?? "").trim().startsWith("https://");
}
