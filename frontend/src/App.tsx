import { Suspense, lazy, useEffect, type ReactNode } from "react";
import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ApiError } from "./api";
import Login from "./pages/Login";
import Confirm from "./pages/Confirm";
import Device from "./pages/Device";
import Dashboard from "./pages/Dashboard";
import Settings, { SystemRedirect } from "./pages/Settings";
import AppDetail from "./pages/AppDetail";
import BasicApp, { BasicHome } from "./pages/BasicApp";
import Layout, { Page } from "./components/Layout";
import { Loading } from "./components/Async";
import { DRIVE, paths } from "./lib/paths";
import { meQuery, personMeQuery, settingsQuery, setupStatusQuery } from "./queries";

// Rarely-first pages load when first visited instead of riding in the
// main bundle: the Federation page is four tabs of tables and dialogs, and
// the app store's spec editor is a page basic-mode households may never
// open. The login screen shouldn't pay for either.
const Federation = lazy(() => import("./pages/Federation"));
const NewApp = lazy(() => import("./pages/NewApp"));
// Files is a whole file manager — its own bundle, loaded when first opened.
const Files = lazy(() => import("./pages/files/Files"));
const Trash = lazy(() => import("./pages/files/Trash"));
// Chat carries the MLS library and the local store — its own bundle. The
// member shell opens the chat client itself, so it rides with it rather
// than putting the MLS library in front of every operator's first paint.
const Chat = lazy(() => import("./pages/Chat"));
const ChatWith = lazy(() => import("./pages/chat/ChatWith"));
const MemberLayout = lazy(() => import("./components/MemberLayout"));
// First-run setup and the join page are each seen once per person. The
// Inbox carries the error-group views it shares with an app's Errors tab,
// so loading it on first visit keeps that tab's code a lazy chunk too.
const Setup = lazy(() => import("./pages/Setup"));
const Join = lazy(() => import("./pages/Join"));
const Inbox = lazy(() => import("./pages/Inbox"));

// While a lazy chunk downloads the shell stays up and the main column says
// what it's doing — same Loading line the page itself would show first.
function LazyPage({ label, children }: { label: string; children: ReactNode }) {
  // The fallback pads itself: a lazy route may be a full-bleed one, and a
  // spinner in the window's top-left corner reads as a broken page.
  return <Suspense fallback={<div className="p-6"><Loading label={label} /></div>}>{children}</Suspense>;
}

// The whole-window line shown while fog works out what to draw.
function Starting() {
  return (
    <div className="flex h-screen items-center justify-center">
      <Loading label="Starting fog…" />
    </div>
  );
}

// The Issues page (and /errors before it) became the Inbox; old links and
// their ?g= / ?tab= deep links follow it there.
function InboxRedirect() {
  const { pathname, search } = useLocation();
  const params = new URLSearchParams(search);
  // The Configuration tab became Issues; ?g= and the rest pass through.
  if (params.get("tab") === "config") params.set("tab", "issues");
  // The Inbox opens on Issues, so a bare /errors (or /issues) has to name
  // its tab — without this, an old ?g= link would land on a tab that has
  // never heard of it.
  if (!params.get("tab")) params.set("tab", pathname === "/errors" ? "errors" : "issues");
  const qs = params.toString();
  return <Navigate to={`/inbox${qs ? `?${qs}` : ""}`} replace />;
}

// Files became Drive, and the folder path is the URL: every link anyone has
// ever sent to a folder is a /files one, so the old prefix still answers and
// carries the rest of the path — and the ?at= of a snapshot — across.
function DriveRedirect() {
  const { pathname, search } = useLocation();
  return <Navigate to={`${DRIVE}${pathname.slice("/files".length)}${search}`} replace />;
}

export default function App() {
  const setup = useQuery(setupStatusQuery);

  // One session, one question: who is signed in. The answer carries the
  // role, which is what decides the shell — a second query asking "…but are
  // they a member?" is what the two logins used to need.
  const me = useQuery({
    ...meQuery,
    enabled: setup.data ? !setup.data.needsSetup : false,
    retry: (count, err) => !(err instanceof ApiError && err.status === 401) && count < 2,
  });

  // The member shell names the household and the person, so it needs the
  // person view — asked for only once the role says that is where we land.
  const personMe = useQuery({
    ...personMeQuery,
    enabled: me.isSuccess && !me.data.operator,
    retry: false,
  });

  // Which shell to render (basic launcher vs full control plane) has to be
  // known before the shell mounts — guessing paints the wrong UI for a beat.
  // If settings won't load, fall through to advanced: its pages carry their
  // own error states, while basic mode would just hide the tools to fix it.
  const settings = useQuery({
    ...settingsQuery,
    enabled: me.isSuccess && me.data.operator,
  });

  // The tab names the server, so several fogs open side by side can be told
  // apart. The member view's household falls back to "this household" when
  // unnamed — that isn't a name, so it doesn't reach the title.
  const household = personMe.data?.household;
  const serverName = (settings.data?.server_name ?? (household === "this household" ? "" : household) ?? "").trim();
  useEffect(() => {
    document.title = serverName ? `${serverName} | Fog` : "Fog";
  }, [serverName]);

  if (setup.isLoading || (setup.data && !setup.data.needsSetup && me.isLoading)) {
    return <Starting />;
  }
  if (setup.isError) {
    return (
      <div className="flex h-screen flex-col items-center justify-center gap-3 px-4 text-center">
        <p className="text-warn">Cannot reach fogd. Is it running?</p>
        <button onClick={() => setup.refetch()}
          className="rounded-md border border-edge px-3 py-1.5 text-sm font-medium text-ink hover:bg-fog-800">
          Try again
        </button>
      </div>
    );
  }

  if (setup.data!.needsSetup) {
    return (
      <Routes>
        <Route path="/setup" element={<LazyPage label="Loading setup…"><Setup /></LazyPage>} />
        <Route path="*" element={<Navigate to="/setup" replace />} />
      </Routes>
    );
  }

  const authed = me.isSuccess;
  if (!authed) {
    return (
      <Routes>
        <Route path="/login" element={<Login />} />
        {/* Confirming needs someone to confirm: sign in first, then back. */}
        <Route path="/confirm" element={<ToLoginKeepingQuery />} />
        <Route path="/device" element={<ToLoginKeepingQuery />} />
        <Route path="/join/:secret" element={<LazyPage label="Loading…"><Join /></LazyPage>} />
        {/* Everyone signs in at the same place, so a member's bookmarked
            room and an operator's dashboard bounce to the one page. */}
        <Route path="*" element={<Navigate to={paths.login} replace />} />
      </Routes>
    );
  }
  // An app asked the signed-in person to confirm it's them (fog sign-in's
  // step-up): the same page for every role, outside any shell.
  if (window.location.pathname === "/confirm") {
    return <Confirm />;
  }
  // Approving a television's sign-in: likewise outside any shell.
  if (window.location.pathname === "/device") {
    return <Device />;
  }
  // Signed in, and not an administrator: the chat and nothing else.
  if (!me.data.operator) {
    if (personMe.isLoading) {
      return <Starting />;
    }
    return (
      <Routes>
        <Route path="/join/:secret" element={<LazyPage label="Loading…"><Join /></LazyPage>} />
        <Route element={
          <Suspense fallback={<Starting />}>
            <MemberLayout household={personMe.data?.household ?? ""} handle={me.data.handle} />
          </Suspense>
        }>
          <Route path="/chat" element={<LazyPage label="Opening chat…"><Chat /></LazyPage>} />
          <Route path="/chat/:roomId" element={<LazyPage label="Opening chat…"><Chat /></LazyPage>} />
        <Route path="/chat/with/:address" element={<LazyPage label="Finding them…"><ChatWith /></LazyPage>} />
          <Route path="/chat/with/:address" element={<LazyPage label="Finding them…"><ChatWith /></LazyPage>} />
          <Route path="*" element={<Navigate to={paths.chat()} replace />} />
        </Route>
      </Routes>
    );
  }

  if (settings.isLoading) {
    return <Starting />;
  }
  const basic = settings.data?.ui_mode === "basic";
  // Basic mode has no triage, fleet, or settings surfaces; those routes fall
  // back home rather than 404.
  const home = <Navigate to={paths.home} replace />;

  return (
    <Routes>
      {/* Joining is public and lives outside the shell: a signed-in
          operator opening a join link sees the same page a newcomer does. */}
      <Route path="/join/:secret" element={<LazyPage label="Loading…"><Join /></LazyPage>} />
      {/* <Page> is the ordinary inset. The three that go without it own
          their own edges: Home's table and Files and the Inbox all run
          their columns to the window. */}
      <Route element={<Layout handle={me.data!.handle} basic={basic} />}>
        <Route path="/" element={basic ? <Page><BasicHome /></Page> : <Dashboard />} />
        <Route path="/inbox" element={basic ? home : <LazyPage label="Loading the inbox…"><Inbox /></LazyPage>} />
        {/* Chat owns the viewport like Files does: rail, timeline, pane. */}
        <Route path="/chat" element={<LazyPage label="Opening chat…"><Chat /></LazyPage>} />
        <Route path="/chat/:roomId" element={<LazyPage label="Opening chat…"><Chat /></LazyPage>} />
        <Route path={`${DRIVE}/*`} element={basic ? home : (
          <LazyPage label="Loading files…"><Files /></LazyPage>
        )} />
        <Route path="/trash" element={basic ? home : (
          <Page><LazyPage label="Loading the trash…"><Trash /></LazyPage></Page>
        )} />
        {/* The Connect page draws its own edge-to-edge bands (tabs,
            footer) and carries the gutter itself, like the app page. */}
        <Route path="/connect" element={basic ? home : (
          <LazyPage label="Loading Connect…"><Federation /></LazyPage>
        )} />
        <Route path="/apps/new" element={<Page><LazyPage label="Loading the app store…"><NewApp /></LazyPage></Page>} />
        {/* The app page draws its own edge-to-edge bands (vitals, health,
            tabs) and carries the gutter itself; basic mode's page is an
            ordinary inset one. */}
        <Route path="/apps/:name" element={basic ? <Page><BasicApp /></Page> : <AppDetail />} />
        <Route path="/apps/:name/:section" element={basic ? <Page><BasicApp /></Page> : <AppDetail />} />
        <Route path="/settings" element={basic ? home : <Page><Settings /></Page>} />
        <Route path="/settings/:area" element={basic ? home : <Page><Settings /></Page>} />
        <Route path="/settings/:area/:page" element={basic ? home : <Page><Settings /></Page>} />

        {/* Old addresses keep working: bookmarks, ntfy links, issue fixes. */}
        <Route path="/files/*" element={<DriveRedirect />} />
        <Route path="/issues" element={<InboxRedirect />} />
        <Route path="/errors" element={<InboxRedirect />} />
        <Route path="/system" element={<SystemRedirect />} />
        <Route path="/system/:page" element={<SystemRedirect />} />
        <Route path="*" element={home} />
      </Route>
    </Routes>
  );
}

// ToLoginKeepingQuery sends someone who is not signed in from /confirm or
// /device to /login: with the same next and app when the page was given
// them, or else back to this very page — so they sign in and carry on.
function ToLoginKeepingQuery() {
  const { pathname, search } = useLocation();
  const q = new URLSearchParams(search);
  if (!q.has("next")) q.set("next", pathname + search);
  return <Navigate to={`${paths.login}?${q}`} replace />;
}
