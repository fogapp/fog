// Where an app's source lives, as a person would open it: the git host
// behind a clone URL, and the page on that host showing the branch Fog
// deploys. Clone URLs are SSH (deploy keys work over nothing else), so the
// web address is inferred — the host's public name, or its SSH user for a
// self-hosted forge, says which forge it is, and each forge has its own
// shape for "this branch's tree".

export type GitProvider = "github" | "gitlab" | "gitea" | "forgejo" | "codeberg" | "bitbucket" | "sourcehut" | "git";

export const PROVIDER_NAMES: Record<GitProvider, string> = {
  github: "GitHub", gitlab: "GitLab", gitea: "Gitea", forgejo: "Forgejo",
  codeberg: "Codeberg", bitbucket: "Bitbucket", sourcehut: "SourceHut", git: "Git",
};

export interface RepoLink {
  provider: GitProvider;
  host: string;
  /** owner/repo as the host shows it — the clone path without its .git */
  path: string;
  /** The branch's tree on the host's web UI. Absent for a repo with no web host (a path on the server). */
  url?: string;
}

// The pieces of an SSH clone URL: user, host, path. Local paths have no host.
function parseClone(repo: string): { user: string; host: string; path: string } | null {
  const r = repo.trim();
  let m = r.match(/^ssh:\/\/(?:([^@/]+)@)?([^/:]+)(?::\d+)?(\/.*)$/);
  if (m) return { user: m[1] ?? "", host: m[2], path: m[3] };
  m = r.match(/^(?:([^@]+)@)?([^:/]+):(.+)$/);
  if (m && !r.startsWith("/")) return { user: m[1] ?? "", host: m[2], path: m[3] };
  return null;
}

function providerOf(user: string, host: string): GitProvider {
  const h = host.toLowerCase();
  if (h === "github.com" || h.endsWith(".github.com")) return "github";
  if (h === "gitlab.com" || h.endsWith(".gitlab.com")) return "gitlab";
  if (h === "bitbucket.org") return "bitbucket";
  if (h === "codeberg.org") return "codeberg";
  if (h === "git.sr.ht") return "sourcehut";
  // A self-hosted forge usually says its name in one of the two places a
  // clone URL has room for: the hostname, or the SSH user it runs as.
  for (const p of ["github", "gitlab", "forgejo", "gitea"] as const) {
    if (h.includes(p) || user.toLowerCase() === p) return p;
  }
  return "git";
}

// A LAN host has no certificate anyone would trust: a bare IP, a single
// label, or a private suffix gets plain http. Everything else gets https.
function webBase(host: string): string {
  const h = host.toLowerCase();
  const lan = /^\d+\.\d+\.\d+\.\d+$/.test(h) || !h.includes(".") || /\.(lan|local|home|internal|home\.arpa)$/.test(h);
  return `${lan ? "http" : "https"}://${host}`;
}

function treeURL(provider: GitProvider, base: string, path: string, branch: string): string {
  const repo = `${base}/${path}`;
  if (!branch) return repo;
  const b = branch.split("/").map(encodeURIComponent).join("/");
  switch (provider) {
    case "github": return `${repo}/tree/${b}`;
    case "gitlab": return `${repo}/-/tree/${b}`;
    case "gitea": case "forgejo": case "codeberg": return `${repo}/src/branch/${b}`;
    case "bitbucket": return `${repo}/src/${b}`;
    case "sourcehut": return `${repo}/tree/${b}`;
    case "git": return repo;
  }
}

/** The app's repository as a place to open. Null when the URL isn't one Fog can read. */
export function repoLink(repo: string, branch = ""): RepoLink | null {
  const r = repo.trim();
  if (!r) return null;
  if (r.startsWith("/")) return { provider: "git", host: "", path: r };
  const c = parseClone(r);
  if (!c) return null;
  const path = c.path.replace(/^\/+/, "").replace(/\/+$/, "").replace(/\.git$/, "");
  if (!path) return null;
  const provider = providerOf(c.user, c.host);
  return { provider, host: c.host, path, url: treeURL(provider, webBase(c.host), path, branch) };
}

// An image app's upstream project as a place to open. The catalog's
// projectUrl is authoritative; failing that, a ghcr.io image names the GitHub
// account that publishes it. linuxserver's images are the exception — their
// repo is the Docker wrapper, not the app — and Docker Hub names say nothing
// reliable, so those stay unlinked rather than guessed.
export function imageProjectLink(image: string, projectUrl?: string): RepoLink | null {
  if (projectUrl) {
    try {
      const u = new URL(projectUrl);
      const path = u.pathname.replace(/^\/+|\/+$/g, "").replace(/\.git$/, "");
      return { provider: providerOf("", u.hostname), host: u.hostname, path, url: projectUrl };
    } catch {
      return null;
    }
  }
  const m = image.trim().match(/^ghcr\.io\/([^/:@]+)\/([^/:@]+)/i);
  if (!m || m[1].toLowerCase() === "linuxserver") return null;
  return { provider: "github", host: "github.com", path: `${m[1]}/${m[2]}`, url: `https://github.com/${m[1]}/${m[2]}` };
}
