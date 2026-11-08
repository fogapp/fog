// The source group heads the standing column for a git app: the forge's
// mark, the repository as owner/repo, and the branch Fog deploys — all one
// link to the branch's tree on the host's web UI, since that is what a
// person wants when they think "where is this app's code". A repo that is
// a path on the server has no page to open and is shown as the path.
// An image app gets the same group pointing at its upstream project, when
// the catalog or the image name says where that is.

import type { AppDetail as AppDetailT } from "../../api";
import ProviderIcon from "../../components/ProviderIcon";
import { imageProjectLink, PROVIDER_NAMES, repoLink, type RepoLink } from "../../lib/gitProvider";
import { useCatalogEntry } from "../../lib/useAppData";
import { Group, SlotLink } from "./cockpit";

// The link the Source group shows, or null when it shows nothing — the
// Version group below needs the same answer to know whether it leads.
export function useSourceLink(name: string, detail: AppDetailT): { link: RepoLink | null; shown: boolean } {
  const { entry } = useCatalogEntry(name);
  const src = detail.spec.source;
  if (src.type === "git") return { link: src.repo ? repoLink(src.repo, src.branch) : null, shown: !!src.repo };
  const link = imageProjectLink(src.image ?? "", entry?.projectUrl);
  return { link, shown: !!link };
}

export function SourceGroup({ name, detail, gap, first = true }: { name: string; detail: AppDetailT; gap: string; first?: boolean }) {
  const src = detail.spec.source;
  const { link, shown } = useSourceLink(name, detail);
  if (!shown) return null;
  const isGit = src.type === "git";
  const branch = src.branch || "default branch";

  return (
    <Group title="Source" first={first} gap={gap} right={link?.url ? <SlotLink to={link.url} external>Open</SlotLink> : undefined}>
      {!link ? (
        <div className="break-all font-mono text-hint text-ink-muted">{src.repo}</div>
      ) : (
        <a
          href={link.url} target="_blank" rel="noreferrer"
          className={`group flex items-center gap-3 ${link.url ? "" : "pointer-events-none"}`}
          aria-disabled={!link.url}
        >
          <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border border-veil-edge bg-veil text-ink transition-colors group-hover:text-accent">
            <ProviderIcon provider={link.provider} />
          </span>
          <span className="min-w-0">
            <span className="block truncate font-mono text-cell text-ink transition-colors group-hover:text-accent" title={link.path}>
              {link.path}
            </span>
            <span className="block truncate text-hint text-ink-muted">
              {PROVIDER_NAMES[link.provider]}{link.host ? ` · ${link.host}` : ""} · {isGit ? <span className="font-mono">{branch}</span> : "upstream project"}
            </span>
          </span>
        </a>
      )}
    </Group>
  );
}
