// Where you are, as a path of names. Derived from the URL alone so the
// header can never disagree with the page under it, and so every level the
// sidebar knows about (Settings › Storage › Disks, hub › Logs, Inbox › Configuration)
// is spelled out rather than collapsed to its top word.

import {
  APP_SECTION_LABELS, INBOX_TAB_LABELS, INBOX_TABS,
  appNameFromPath, appSectionFromPath, filesRelFromPath, paths, type InboxTab,
} from "./paths";
import { settingsArea, settingsPage, type SettingsTo } from "./settings";

export interface Crumb {
  label: string;
  to?: string;
}

export function crumbsFor(pathname: string, search: string, opts: { basic: boolean }): Crumb[] {
  if (pathname === paths.home) return [{ label: opts.basic ? "Apps" : "Home" }];
  if (pathname === paths.newApp) return [{ label: "Add app" }];

  const app = appNameFromPath(pathname);
  if (app) {
    const section = appSectionFromPath(pathname);
    return section
      ? [{ label: app, to: paths.app(app) }, { label: APP_SECTION_LABELS[section] }]
      : [{ label: app }];
  }

  if (pathname === paths.federation) return [{ label: "Connect" }];

  // Chat names the room in its own header, the way Drive names the folder.
  if (pathname === "/chat" || pathname.startsWith("/chat/")) return [{ label: "Chat" }];

  if (pathname === paths.trash) return [{ label: "Drive", to: paths.files() }, { label: "Trash" }];

  // Drive is the exception: the folder path is spelled by the page's own
  // path bar, pinned under the listing it describes, so the header says
  // only which page you are on.
  if (filesRelFromPath(pathname) !== undefined) return [{ label: "Drive" }];

  if (pathname.startsWith("/inbox")) {
    const raw = new URLSearchParams(search).get("tab") ?? "";
    const tab = (INBOX_TABS as readonly string[]).includes(raw) ? (raw as InboxTab) : INBOX_TABS[0];
    return [{ label: "Inbox", to: paths.inbox() }, { label: INBOX_TAB_LABELS[tab] }];
  }

  if (pathname === "/settings" || pathname.startsWith("/settings/")) {
    const [areaId = "", pageId = ""] = pathname.slice("/settings/".length).split("/");
    const area = settingsArea(areaId);
    const page = area && settingsPage(areaId, pageId);
    if (!area || !page) return [{ label: "Settings" }];
    return [
      { label: "Settings", to: paths.settings() },
      { label: area.label, to: paths.settings(area.id as SettingsTo) },
      { label: page.label },
    ];
  }

  return [];
}
