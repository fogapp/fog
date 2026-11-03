// Settings: the overview at /settings, and every page of every area at
// /settings/<area>/<page> inside the frame that lists the area's pages.
// This file is only the switch; each page lives in ./settings, and the
// map of areas and pages is lib/settings.ts.

import { Suspense, lazy, type ComponentType, type LazyExoticComponent } from "react";
import { Navigate, useLocation, useParams } from "react-router-dom";
import { Loading } from "../components/Async";
import { paths } from "../lib/paths";
import {
  fromOldSettingsTab, fromSystemPath, settingsArea, settingsPage, type SettingsAreaId, type SettingsTo,
} from "../lib/settings";
import { SettingsFrame } from "./settings/Frame";

type PageTo = Exclude<SettingsTo, SettingsAreaId>;

// Each page is its own chunk, fetched when first opened. Keyed by the
// Settings map's own addresses, so a page in the map with no component
// here is a compile error rather than a blank frame.
const PAGES: Record<PageTo, LazyExoticComponent<ComponentType>> = {
  "server/about": lazy(() => import("./settings/About")),
  "server/updates": lazy(() => import("./settings/Updates")),
  "server/docker": lazy(() => import("./settings/Docker")),
  "server/power": lazy(() => import("./settings/Power")),
  "server/compute": lazy(() => import("./settings/Compute")),
  "server/activity": lazy(() => import("./settings/Activity")),
  "network/domain": lazy(() => import("./settings/Domain")),
  "network/proxy": lazy(() => import("./settings/Proxy")),
  "network/tunnel": lazy(() => import("./settings/Tunnel")),
  "network/away": lazy(() => import("./settings/Away")),
  "storage/disks": lazy(() => import("./settings/Disks")),
  "storage/drive": lazy(() => import("./settings/Drive")),
  "storage/database": lazy(() => import("./settings/Database")),
  "storage/backups": lazy(() => import("./settings/Backups")),
  "storage/offsite": lazy(() => import("./settings/Offsite")),
  "storage/recovery": lazy(() => import("./settings/Recovery")),
  "storage/orphans": lazy(() => import("./settings/Orphans")),
  "access/people": lazy(() => import("./settings/People")),
  "access/signin": lazy(() => import("./settings/SignIn")),
  "access/ssh": lazy(() => import("./settings/Ssh")),
  "access/ai": lazy(() => import("./settings/Ai")),
  "preferences/appearance": lazy(() => import("./settings/Appearance")),
  "preferences/notifications": lazy(() => import("./settings/Notifications")),
  "preferences/app-store": lazy(() => import("./settings/AppStore")),
};

const Overview = lazy(() => import("./settings/Overview"));

const fallback = <Loading label="Loading…" />;

export default function Settings() {
  const { area: areaId = "", page: pageId = "" } = useParams();
  if (!areaId) {
    return (
      <Suspense fallback={fallback}>
        <Overview />
      </Suspense>
    );
  }
  const area = settingsArea(areaId);
  if (!area) return <Navigate to={fromOldSettingsTab(areaId) ?? paths.settings()} replace />;
  const page = settingsPage(areaId, pageId);
  if (!page) return <Navigate to={paths.settings(`${area.id}/${area.pages[0].id}` as SettingsTo)} replace />;
  const Page = PAGES[`${area.id}/${page.id}` as PageTo];
  return (
    <SettingsFrame area={area} page={page}>
      <Suspense fallback={fallback}>
        <Page />
      </Suspense>
    </SettingsFrame>
  );
}

// /system/<page>#<section>, the address the Settings area had before.
export function SystemRedirect() {
  const { page = "" } = useParams();
  const { hash } = useLocation();
  return <Navigate to={fromSystemPath(page, hash)} replace />;
}
