// Every in-app path is built here. A string literal like "/apps/x/errors"
// scattered across thirty call sites is how a route rename leaves a dead
// link in the one place nobody re-read; a builder can be renamed once and
// the compiler finds the rest. The route table in App.tsx, the sidebar and
// the breadcrumb all read from this vocabulary.

import type { SettingsTo } from "./settings";

// The app page's sections, as URL segments. The first nine are the tab
// strip, in its order; history and terminal are reached from the masthead's
// More… menu and from links, and stay addressable so a shared link or the
// palette can land on them. "backups" is no longer a section — the app page
// redirects it to Drive, where restore points now live.
export const APP_SECTIONS = ["logs", "activity", "drive", "connect", "signin", "integrations", "updates", "config", "errors", "history", "terminal"] as const;
export type AppSection = (typeof APP_SECTIONS)[number];
export const APP_TABS = ["overview", "drive", "connect", "signin", "integrations", "updates", "config", "errors", "logs", "activity"] as const;

export const APP_SECTION_LABELS: Record<"overview" | AppSection, string> = {
  overview: "Overview",
  logs: "Logs",
  activity: "Activity",
  drive: "Drive",
  connect: "Connect",
  signin: "Sign-in",
  integrations: "Integrations",
  updates: "Updates",
  config: "Config",
  errors: "Errors",
  history: "History",
  terminal: "Terminal",
};

// Issues first: its list is headed by notices, the only kind that waits on
// a person by definition, so it is what the Inbox should open on. Notices
// had a tab of their own until they folded into it. Resolved is the
// archive — acknowledged and self-resolved issues, which used to hide
// behind sub-tabs on Issues.
export const INBOX_TABS = ["issues", "errors", "feedback", "resolved"] as const;
export type InboxTab = (typeof INBOX_TABS)[number];
export const INBOX_TAB_LABELS: Record<InboxTab, string> = {
  issues: "Issues", errors: "Errors", feedback: "Feedback", resolved: "Resolved",
};

function withSearch(path: string, params?: Record<string, string | undefined>): string {
  if (!params) return path;
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "") sp.set(k, v);
  const s = sp.toString();
  return s ? `${path}?${s}` : path;
}

// DRIVE is the Drive page's route prefix. It was /files until the page was
// named Drive; App.tsx still answers the old prefix with a redirect, so
// bookmarks and links people have already sent each other keep working.
export const DRIVE = "/drive";

// The Federation page's views, in strip order. Peers is the default and
// is written as the absence of ?tab=; the selected peer rides along as
// ?peer=<id> so a sidebar link or a shared address lands on that peer.
// Config is this fog's own side of federation — identity, address,
// reachability, the push preference.
export const FED_TABS = ["peers", "sharing", "invites", "activity", "config"] as const;
export type FedTab = (typeof FED_TABS)[number];
export const FED_TAB_LABELS: Record<FedTab, string> = {
  peers: "Peers", sharing: "Sharing", invites: "Invites", activity: "Activity", config: "Config",
};

export const paths = {
  home: "/",
  login: "/login",
  // A join link from Settings › People. Public: the person has no account yet.
  join: (secret: string) => `/join/${encodeURIComponent(secret)}`,
  // Chat: the room is the path, the open thread rides along as ?thread=.
  chat: (roomId?: string, params?: Record<string, string | undefined>) =>
    withSearch(`/chat${roomId ? `/${encodeURIComponent(roomId)}` : ""}`, params),
  // Member sign-in: people who are not the operator land here.
  federation: "/connect",
  federationView: (tab?: FedTab, params?: Record<string, string | undefined>) =>
    withSearch("/connect", { ...(tab && tab !== FED_TABS[0] ? { tab } : {}), ...params }),
  newApp: "/apps/new",
  // An app's listing in the store — the page a card opens.
  storeApp: (id: string) => withSearch("/apps/new", { app: id }),

  // Drive: the folder path is the URL, one encoded segment per folder, so
  // every folder is a link and the breadcrumb spells the whole path.
  // at names a snapshot to browse instead of the live tree (time travel).
  files: (rel = "", at?: number) =>
    `${DRIVE}${rel ? "/" + rel.split("/").map(encodeURIComponent).join("/") : ""}${at ? `?at=${at}` : ""}`,
  // The Drive tab that is a view rather than a folder: what peers can
  // reach, gathered from wherever in the tree it lives. A flag on the root
  // URL, not a path segment, so it can never collide with a real folder
  // someone names Shared.
  filesShared: () => `${DRIVE}?view=shared`,
  trash: "/trash",

  inbox: (tab?: InboxTab, params?: Record<string, string | undefined>) =>
    withSearch("/inbox", { ...(tab && tab !== INBOX_TABS[0] ? { tab } : {}), ...params }),

  app: (name: string, section?: AppSection, params?: Record<string, string | undefined>) =>
    withSearch(`/apps/${encodeURIComponent(name)}${section ? `/${section}` : ""}`, params),

  // Settings: /settings is the overview; "storage/disks" a page, with an
  // optional anchor for the exact control (#policy).
  settings: (to?: SettingsTo, anchor?: string) =>
    `/settings${to ? `/${to}` : ""}${anchor ? `#${anchor}` : ""}`,
};

// Read an app name back out of a pathname. Undefined on every page that is
// not an app page, including the store.
export function appNameFromPath(pathname: string): string | undefined {
  const m = pathname.match(/^\/apps\/([^/]+)/);
  if (!m || m[1] === "new") return undefined;
  return decodeURIComponent(m[1]);
}

export function appSectionFromPath(pathname: string): AppSection | undefined {
  const s = pathname.match(/^\/apps\/[^/]+\/([^/]+)/)?.[1];
  return (APP_SECTIONS as readonly string[]).includes(s ?? "") ? (s as AppSection) : undefined;
}

// The root-relative folder a /drive URL names; undefined off the Drive page.
export function filesRelFromPath(pathname: string): string | undefined {
  if (pathname !== DRIVE && !pathname.startsWith(DRIVE + "/")) return undefined;
  return pathname
    .slice(DRIVE.length)
    .split("/")
    .filter(Boolean)
    .map((s) => {
      try {
        return decodeURIComponent(s);
      } catch {
        return s;
      }
    })
    .join("/");
}
