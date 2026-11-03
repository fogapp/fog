// The Settings frame. An area (Storage) is a short list of pages (Disks &
// shares, Drive, Backups…): the list stands at the left of the page, the
// page fills the rest, and only one subject is ever on screen. Below the
// lg breakpoint the list becomes the pill strip every other page uses for
// peer views.
//
// Every page opens the same way — SettingsPage: its name, one line on what
// it is for, its status, the actions that belong to the whole page — so
// the title, the answer and the buttons sit in the same place on every
// page instead of each one deciding where.

import { createContext, useContext, useEffect, type ReactNode } from "react";
import { NavLink, useLocation } from "react-router-dom";
import PageHeader from "../../components/PageHeader";
import { SettingsBadge } from "../../components/SettingsBadges";
import Tabs from "../../components/Tabs";
import { paths } from "../../lib/paths";
import type { SettingsArea, SettingsPageDef, SettingsTo } from "../../lib/settings";

const Here = createContext<{ area: SettingsArea; page: SettingsPageDef } | null>(null);

const itemClass = ({ isActive }: { isActive: boolean }) =>
  `flex h-8 items-center gap-2 rounded-md px-3 text-sm transition-colors ${
    isActive ? "bg-accent/10 font-medium text-accent" : "text-ink-muted hover:bg-hover hover:text-ink"
  }`;

export function SettingsFrame({ area, page, children }: { area: SettingsArea; page: SettingsPageDef; children: ReactNode }) {
  useHashLanding();
  const to = (p: SettingsPageDef) => paths.settings(`${area.id}/${p.id}` as SettingsTo);
  return (
    <Here.Provider value={{ area, page }}>
      <div className="flex gap-10">
        <nav aria-label={`${area.label} settings`} className="sticky top-20 hidden w-48 shrink-0 self-start lg:block">
          <p className="group-label px-3 pb-2">{area.label}</p>
          <ul className="space-y-0.5">
            {area.pages.map((p) => (
              <li key={p.id}>
                <NavLink to={to(p)} className={itemClass}>
                  <span className="min-w-0 flex-1 truncate">{p.label}</span>
                  <SettingsBadge to={`${area.id}/${p.id}`} />
                </NavLink>
              </li>
            ))}
          </ul>
        </nav>
        <div className="min-w-0 max-w-4xl flex-1">
          <div className="mb-6 lg:hidden">
            <Tabs label={`${area.label} settings`} items={area.pages.map((p) => ({ id: p.id, label: p.label, to: to(p) }))} />
          </div>
          {children}
        </div>
      </div>
    </Here.Provider>
  );
}

// The top of a Settings page. Title and description come from the
// Settings map unless the page has something more particular to say;
// status is the StatusLine that answers "is this working?".
export function SettingsPage({
  title,
  description,
  status,
  actions,
  children,
}: {
  title?: ReactNode;
  description?: ReactNode;
  status?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  const here = useContext(Here);
  return (
    <div className="space-y-10">
      <PageHeader size="md" title={title ?? here?.page.label} description={description ?? here?.page.blurb} actions={actions}>
        {status}
      </PageHeader>
      {children}
    </div>
  );
}

// Arriving with #policy: scroll there once the section has mounted. A
// page's sections often wait on a query, so look for a moment before
// giving up; a page change without a hash starts at the top.
function useHashLanding() {
  const { pathname, hash } = useLocation();
  useEffect(() => {
    if (!hash) {
      window.scrollTo({ top: 0 });
      return;
    }
    let tries = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const look = () => {
      const el = document.getElementById(decodeURIComponent(hash.slice(1)));
      if (el) el.scrollIntoView({ block: "start", behavior: "smooth" });
      else if (tries++ < 30) timer = setTimeout(look, 100);
    };
    look();
    return () => clearTimeout(timer);
  }, [pathname, hash]);
}
