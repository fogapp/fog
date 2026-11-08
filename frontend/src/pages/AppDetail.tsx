// The app page: the cockpit. Two things never move — the masthead and the
// 24-hour health strip sit above the tabs and stay put, so "is it okay
// right now" is never more than a glance away. Below
// them, Overview pairs what you came to look at with a standing column of
// facts that are always true; every other tab takes the full width. The
// shell holds the shared queries, the actions, the modals and the keys;
// each tab lives in ./app.

import { Suspense, lazy, useState } from "react";
import { Navigate, useLocation, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, post, type AppVersion } from "../api";
import { appsKey, useApps } from "../lib/useApps";
import SetupPanel from "../components/SetupPanel";

import PeerShareComposer from "../components/files/PeerShareComposer";
import { LoadError, Loading } from "../components/Async";
import { routeHost } from "../lib/hostname";
import { APP_SECTION_LABELS, APP_SECTIONS, APP_TABS, paths, type AppSection } from "../lib/paths";
import { useAppContainer, useAppDetail, useAppVolumes, useAppBackups } from "../lib/useAppData";
import { AppSubheader, MoreMenu, UninstallModal, UpdateModal, useAppActions, type AppModal } from "./app/Actions";
import { FirstDeploy, Overview } from "./app/Overview";
import { ImportReview } from "./app/ImportReview";
import { StandingColumn } from "./app/Standing";
import { GUTTER } from "./app/cockpit";
import { settingsQuery } from "../queries";
import { useHotkeys } from "../lib/useHotkeys";

// Overview is where the page opens; every other tab downloads on first
// open. The terminal is the only place that needs the xterm emulator, and
// Config carries the spec editor.
const Terminal = lazy(() => import("../components/Terminal"));
const Config = lazy(() => import("./app/Config").then((m) => ({ default: m.Config })));
const Logs = lazy(() => import("./app/Logs").then((m) => ({ default: m.Logs })));
const ActivityTab = lazy(() => import("./app/Activity").then((m) => ({ default: m.ActivityTab })));
const DriveTab = lazy(() => import("./app/Drive").then((m) => ({ default: m.DriveTab })));
const FederationTab = lazy(() => import("./app/Federation").then((m) => ({ default: m.FederationTab })));
const SignInTab = lazy(() => import("./app/SignIn").then((m) => ({ default: m.SignInTab })));
const IntegrationsTab = lazy(() => import("./app/Integrations").then((m) => ({ default: m.IntegrationsTab })));
const Updates = lazy(() => import("./app/Updates").then((m) => ({ default: m.Updates })));
const RollbackModal = lazy(() => import("./app/Updates").then((m) => ({ default: m.RollbackModal })));
const History = lazy(() => import("./app/History").then((m) => ({ default: m.History })));
const Errors = lazy(() => import("./app/Errors").then((m) => ({ default: m.Errors })));

// Every addressable section, from the one list paths.ts keeps.
const SECTIONS: readonly string[] = ["overview", ...APP_SECTIONS];
type Tab = "overview" | AppSection;

export default function AppDetail() {
  // Sections are URL segments (/apps/:name/:section) so the sidebar can link
  // to them and a refresh lands where you were. No section means overview.
  const { name = "", section } = useParams();
  const location = useLocation();
  const navigate = useNavigate();
  const tab: Tab = SECTIONS.includes(section ?? "") ? (section as Tab) : "overview";
  const qc = useQueryClient();
  const [modal, setModal] = useState<AppModal>(null);
  const [sharePath, setSharePath] = useState<string | undefined>(undefined);
  // The version the rollback dialog is about. It lives here, with the other
  // dialogs, so the page's keys stand down while it's open — "r" typed at a
  // confirm dialog must not restart the app behind it.
  const [rollbackTo, setRollbackTo] = useState<AppVersion | null>(null);

  const { detail, deployStatus, deploying, refresh, awaitDeploy } = useAppDetail(name);
  const statuses = useApps({ stats: true, refetchInterval: 5000 });
  const status = statuses.data?.find((s) => s.name === name);
  const neverDeployed = detail.data ? detail.data.app.currentRev === 0 : false;
  const container = useAppContainer(name, !!detail.data && !neverDeployed);

  const settings = useQuery(settingsQuery);
  const { volumes, filesStatus } = useAppVolumes(detail.data);
  const backups = useAppBackups(name);

  const actions = useAppActions({ name, status, statusKnown: !!statuses.data, deploying, onAction: refresh, awaitDeploy });

  const go = (s: AppSection, params?: Record<string, string>) => navigate(paths.app(name, s, params));

  // R restart, U update, L logs, D drive, F federation; none while a
  // dialog is open.
  useHotkeys(
    {
      r: () => {
        if (actions.ready) actions.act(actions.running ? "restart" : "start");
      },
      u: () => {
        if (status?.updateAvailable && actions.ready) setModal("update");
      },
      l: () => go("logs"),
      d: () => go("drive"),
      f: () => go("connect"),
    },
    { disabled: !!modal },
  );

  // The old Backups tab: its address keeps working and lands on Drive,
  // where restore points and the schedule now live (an issue's fix link
  // still says ?tab=schedule).
  if (section === "backups") {
    return <Navigate to={`${paths.app(name, "drive")}${location.search}`} replace />;
  }
  // An unknown section isn't a page — snap back to the app's overview.
  if (section && !SECTIONS.includes(section)) {
    return <Navigate to={paths.app(name)} replace />;
  }

  // "App not found" is a claim about the server's state — reserve it for an
  // actual 404. A failed request means we don't know, and says so.
  if (!detail.data) {
    return (
      <div className={`${GUTTER} py-6`}>
        {!detail.isError ? (
          <Loading label={`Loading ${name}…`} />
        ) : detail.error instanceof ApiError && detail.error.status === 404 ? (
          <p className="text-ink-muted">No app named {name}. It may have been deleted.</p>
        ) : (
          <LoadError label={`Couldn't load ${name}.`} error={detail.error} onRetry={() => detail.refetch()} />
        )}
      </div>
    );
  }

  const isGit = detail.data.spec.source.type === "git";
  const routes = detail.data.spec.routes ?? [];
  const primaryHost = status?.hosts?.[0] ?? (routes.length > 0 ? routeHost(name, routes[0].host, settings.data?.base_domain) : null);
  // Prefer the HTTPS hostname; fall back to the direct port on the server.
  const openURL = primaryHost
    ? `https://${primaryHost}`
    : status?.hostPort
      ? `http://${window.location.hostname}:${status.hostPort}`
      : null;
  const openable = status?.state === "running" && !!openURL;
  const driveFolders = volumes.flatMap((v) => (v.rel !== undefined ? [v.rel] : []));
  const statusStale = statuses.isError && !!statuses.data;

  const fill = tab === "logs" && !neverDeployed;

  const cancelRestore = () =>
    post(`/api/apps/${name}/restore/cancel`).then(() => qc.invalidateQueries({ queryKey: appsKey }));

  return (
    // The logs tab is pinned to the viewport so its pane can take the rest
    // of the frame and scroll inside it; every other tab grows with the page.
    <div className={`flex ${fill ? "h-[calc(100dvh-3.5rem)]" : "min-h-[calc(100vh-3.5rem)]"} flex-col`}>
      {!neverDeployed && (
        <AppSubheader
          openURL={openable ? openURL : null}
          status={status}
          statusKnown={!!statuses.data}
          statusError={statuses.isError}
          actions={actions}
          onMore={() => setModal("more")}
          stale={statusStale}
          items={APP_TABS.map((t) => ({
            id: t,
            label: APP_SECTION_LABELS[t],
            to: paths.app(name, t === "overview" ? undefined : t),
            end: t === "overview",
            ...(t === "errors" && status?.openErrors ? { count: status.openErrors, hot: true } : {}),
          }))}
        />
      )}

      {neverDeployed ? (
        <div className={`${GUTTER} flex-1 py-6`}>
          {detail.data.reviewPending ? (
            <ImportReview name={name} isGit={isGit} onConfirmed={refresh} onRemove={() => setModal("uninstall")} />
          ) : isGit ? (
            <SetupPanel key={name} name={name} detail={detail.data} deployStatus={deployStatus.data} onDeployStarted={refresh}
              onRemove={() => setModal("uninstall")} />
          ) : (
            <FirstDeploy name={name} detail={detail.data} deployStatus={deployStatus.data}
              deployError={deployStatus.isError} onAction={refresh} onRemove={() => setModal("uninstall")} />
          )}
        </div>
      ) : (
        <>
          {/* Children that hold app-scoped state — drafts, sockets, buffered
              log lines — are keyed by app name: when the next app's detail is
              already cached nothing here unmounts on its own, and an unkeyed
              child would keep showing one app's state under another's
              subheader. */}
          <div className={`grid min-h-0 flex-1 ${tab === "overview" ? "lg:grid-cols-[minmax(0,1fr)_340px]" : ""}`}>
            <div className={`min-w-0 ${GUTTER} py-[26px] pb-10 ${fill ? "flex min-h-0 flex-col" : ""}`}>
              {tab === "overview" && (
                <Overview name={name} detail={detail.data} deployStatus={deployStatus.data} />
              )}
              <Suspense fallback={<Loading label={`Loading ${APP_SECTION_LABELS[tab].toLowerCase()}…`} />}>
                {tab === "logs" && <Logs key={name} name={name} />}
                {tab === "activity" && <ActivityTab name={name} />}
                {tab === "drive" && (
                  <DriveTab key={name} name={name} detail={detail.data} volumes={volumes} filesStatus={filesStatus.data}
                    restorePending={status?.restorePending} onSaved={refresh}
                    onShare={(p) => { setSharePath(p); setModal("share"); }} />
                )}
                {tab === "signin" && <SignInTab key={name} name={name} />}
                {tab === "integrations" && <IntegrationsTab key={name} name={name} onSaved={refresh} />}
                {tab === "connect" && (
                  <FederationTab name={name} detail={detail.data} status={status} volumes={volumes}
                    openURL={openURL} onShare={(p) => { setSharePath(p); setModal("share"); }} />
                )}
                {tab === "updates" && (
                  <Updates key={name} name={name} detail={detail.data} status={status} actions={actions}
                    onUpdate={() => setModal("update")}
                    onRollback={(v) => { setRollbackTo(v); setModal("rollback"); }} />
                )}
                {tab === "config" && <Config key={name} name={name} detail={detail.data} onSaved={refresh} />}
                {tab === "errors" && <Errors key={name} name={name} />}
                {tab === "history" && <History key={name} name={name} detail={detail.data} onAction={refresh} />}
                {tab === "terminal" && <Terminal key={name} appName={name} />}
              </Suspense>
            </div>
            {tab === "overview" && (
              <StandingColumn
                name={name} detail={detail.data} status={status} statusKnown={!!statuses.data} container={container.data}
                containerError={container.isError} deployStatus={deployStatus.data} volumes={volumes} filesStatus={filesStatus.data}
                actions={actions} onOpen={setModal} onGoDrive={() => go("drive")} onGoFed={() => go("connect")}
                onGoLogs={() => go("logs")} onCancelRestore={cancelRestore} onSaved={refresh}
              />
            )}
          </div>
        </>
      )}

      {modal === "more" && (
        <MoreMenu name={name} actions={actions} onClose={() => setModal(null)} onUninstall={() => setModal("uninstall")} />
      )}
      {modal === "update" && (
        <UpdateModal name={name} detail={detail.data} status={status} onClose={() => setModal(null)} onUpdate={actions.update} />
      )}
      {modal === "uninstall" && (
        <UninstallModal name={name} driveFolders={driveFolders} snapshots={backups.data?.length} actions={actions}
          onClose={() => setModal(null)} neverInstalled={neverDeployed} />
      )}
      {modal === "rollback" && rollbackTo && (
        <Suspense fallback={null}>
          <RollbackModal name={name} version={rollbackTo} detail={detail.data}
            onClose={() => setModal(null)} onRollback={actions.rollback} />
        </Suspense>
      )}
      {modal === "share" && <PeerShareComposer path={sharePath} onClose={() => setModal(null)} />}
    </div>
  );
}
