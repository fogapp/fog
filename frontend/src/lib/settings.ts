// The Settings map: five areas, each a short list of focused pages. This
// is the one place the shape is written down — the sidebar lists the
// areas, the settings frame lists an area's pages beside the page, the
// breadcrumb spells "Settings / Storage / Disks & shares", the palette
// offers every page by name and by the words people search for, and the
// redirects from the old /system addresses land on these. Rename a page
// here and every one of them follows.

export const SETTINGS_AREAS = [
  {
    id: "server",
    label: "Server",
    pages: [
      { id: "about", label: "About", blurb: "This server's name, and the machine it runs on.", terms: "server name hostname identity version uptime" },
      { id: "updates", label: "Updates", blurb: "Fog's own releases, and newer versions of every app.", terms: "self-update release upgrade apps" },
      { id: "docker", label: "Docker", blurb: "The engine every app runs in.", terms: "containers images engine" },
      { id: "power", label: "Power", blurb: "Reboot or shut down the machine.", terms: "reboot shutdown restart" },
      { id: "compute", label: "Compute", blurb: "Other machines that lend fog their hardware: transcoding, AI models.", terms: "mac studio gpu transcode videotoolbox node fog-node inference" },
      { id: "activity", label: "Activity log", blurb: "Who did what on this server, and when.", terms: "audit log history" },
    ],
  },
  {
    id: "network",
    label: "Network",
    pages: [
      { id: "domain", label: "Domain & HTTPS", blurb: "One domain gives every app a clean address with real HTTPS.", terms: "dns cloudflare certificate reachability port forwarding" },
      { id: "proxy", label: "Proxy", blurb: "Caddy, the reverse proxy in front of every app.", terms: "caddy acme certificates" },
      { id: "tunnel", label: "Cloudflare Tunnel", blurb: "Reach chosen apps from the internet without opening ports.", terms: "cloudflared remote" },
      { id: "away", label: "Away access", blurb: "Reach this box from anywhere over your own tailnet.", terms: "tailscale vpn tailnet remote" },
    ],
  },
  {
    id: "storage",
    label: "Storage",
    pages: [
      { id: "disks", label: "Disks & shares", blurb: "Every filesystem fog writes to, and what each one holds.", terms: "disk nas nfs smb cifs mount network share space" },
      { id: "drive", label: "Drive", blurb: "Where fog's own files live, and the devices that mount them.", terms: "files webdav finder explorer app password" },
      { id: "database", label: "Database", blurb: "The shared Postgres cluster every database-backed app uses.", terms: "postgres cluster upgrade" },
      { id: "backups", label: "Backups", blurb: "Is everything backed up — and the policy every app falls back to.", terms: "snapshot schedule retention policy" },
      { id: "offsite", label: "Offsite", blurb: "Copies of every backup, kept off this machine.", terms: "backblaze b2 s3 locker cloud" },
      { id: "recovery", label: "Recovery kit", blurb: "What it takes to rebuild this server from nothing.", terms: "passphrase export manifests disaster" },
      { id: "orphans", label: "Orphaned data", blurb: "Data on disk that no installed app owns.", terms: "reclaim leftovers cleanup" },
    ],
  },
  {
    id: "access",
    label: "People & access",
    pages: [
      { id: "people", label: "People", blurb: "Everyone with an account here, and the links that bring new people in.", terms: "invite members accounts household" },
      { id: "signin", label: "Your sign-in", blurb: "The passkeys and sessions that sign you in.", terms: "passkey password sessions security" },
      { id: "ssh", label: "SSH", blurb: "Keys that can log in to the machine itself.", terms: "ssh keys root shell" },
      { id: "ai", label: "AI assistants", blurb: "Let an assistant like Claude Code look after this server.", terms: "mcp claude tokens ai" },
    ],
  },
  {
    id: "preferences",
    label: "Preferences",
    pages: [
      { id: "appearance", label: "Appearance", blurb: "How fog looks, and how much of it the household sees.", terms: "theme dark light palette basic advanced interface mode" },
      { id: "notifications", label: "Notifications", blurb: "How fog reaches your phone when something needs you.", terms: "ntfy push alerts" },
      { id: "app-store", label: "App store", blurb: "Which apps are offered under Add app.", terms: "catalog store" },
    ],
  },
] as const;

type Areas = (typeof SETTINGS_AREAS)[number];
export type SettingsAreaId = Areas["id"];
export type SettingsArea = Areas;
export type SettingsPageDef = Areas["pages"][number];

// "storage/disks" — an area and one of its own pages, checked by the
// compiler so a link can only name a page that exists. A bare area opens
// its first page.
type PageOf<A> = A extends { id: infer I extends string; pages: readonly { id: infer P extends string }[] } ? `${I}/${P}` : never;
export type SettingsTo = SettingsAreaId | PageOf<Areas>;

export function settingsArea(id: string): SettingsArea | undefined {
  return SETTINGS_AREAS.find((a) => a.id === id);
}

export function settingsPage(area: string, page: string): SettingsPageDef | undefined {
  return (settingsArea(area)?.pages as readonly SettingsPageDef[] | undefined)?.find((p) => p.id === page);
}

// "Settings › Storage › Backups", for prose that tells someone where to go.
export function settingsLabel(to: SettingsTo): string {
  const [a, p] = to.split("/");
  const area = settingsArea(a);
  const page = p ? settingsPage(a, p) : undefined;
  return ["Settings", area?.label, page?.label].filter(Boolean).join(" › ");
}

// Where every address the old System area had now lives. Bookmarks, ntfy
// pushes and stored issue fixes still carry /system/<page>#<section>; each
// old section has one new home, and a page with no section named lands on
// the page its first section moved to. Anchors that survive the move (the
// palettes on Appearance, #policy on Backups) are carried across.
type Dest = readonly [SettingsTo | "", string?];
const SYSTEM_MOVES: Record<string, { page: Dest; sections?: Record<string, Dest> }> = {
  "": {
    page: [""],
    sections: { identity: ["server/about"], interface: ["preferences/appearance", "interface"], power: ["server/power"] },
  },
  dependencies: {
    page: [""],
    sections: {
      docker: ["server/docker"], postgres: ["storage/database"], proxy: ["network/proxy"],
      tunnel: ["network/tunnel"], vpn: ["network/away"], ntfy: ["preferences/notifications", "ntfy"],
    },
  },
  network: { page: ["network/domain"] },
  storage: {
    page: ["storage/disks"],
    sections: { "files-access": ["storage/drive", "access"], orphans: ["storage/orphans"], postgres: ["storage/database"] },
  },
  backups: {
    page: ["storage/backups"],
    sections: { policy: ["storage/backups", "policy"], offsite: ["storage/offsite"], "recovery-kit": ["storage/recovery"], export: ["storage/recovery", "export"] },
  },
  updates: { page: ["server/updates"], sections: { fog: ["server/updates", "fog"], apps: ["server/updates", "apps"] } },
  notifications: { page: ["preferences/notifications"] },
  people: { page: ["access/people"] },
  security: { page: ["access/signin"], sections: { ssh: ["access/ssh"] } },
  appearance: { page: ["preferences/appearance"], sections: { mode: ["preferences/appearance", "mode"], light: ["preferences/appearance", "light"], dark: ["preferences/appearance", "dark"] } },
  "app-store": { page: ["preferences/app-store"] },
  ai: { page: ["access/ai"] },
  activity: { page: ["server/activity"] },
};

// Before System there was a Settings page with tabs (/settings/<tab>);
// "storage" is an area again, the rest are spelled here.
const OLD_SETTINGS_TABS: Record<string, string> = {
  general: "/settings/server/about",
  advanced: "/settings",
  domain: "/settings/network/domain",
  apps: "/settings/preferences/app-store",
  updates: "/settings/server/updates",
  ai: "/settings/access/ai",
  activity: "/settings/server/activity",
  peers: "/connect",
  issues: "/inbox",
};

// The new address for /system/<page><#hash>.
export function fromSystemPath(page: string, hash: string): string {
  const move = SYSTEM_MOVES[page];
  if (!move) return "/settings";
  const [to, anchor] = move.sections?.[hash.replace(/^#/, "")] ?? move.page;
  return `/settings${to ? `/${to}` : ""}${anchor ? `#${anchor}` : ""}`;
}

// The new address for an old /settings/<tab>, or undefined when the
// segment is not one of them.
export function fromOldSettingsTab(tab: string): string | undefined {
  return OLD_SETTINGS_TABS[tab];
}
