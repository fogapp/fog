import { useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { post } from "../api";
import { errorText } from "./Async";
import { useDismissable } from "../lib/useDismissable";
import { useSettingToggle } from "../lib/useSettingToggle";
import { useTheme, type ThemePref } from "../theme";
import { paths } from "../lib/paths";
import { meQuery } from "../queries";

// The header's account corner: an initial-letter avatar opening a small
// menu with the things that are about the person at the keyboard rather
// than the box — appearance, which mode of fog they want, sign out. Fog is
// one administrator in practice, so the "profile" is just the handle.
//
// The mode switch lives here because it has to be reachable from basic
// mode, which hides everything else: one click to the menu, one to switch.
export default function AvatarMenu({ handle, basic }: { handle: string; basic?: boolean }) {
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  const qc = useQueryClient();
  const navigate = useNavigate();
  const { pref, setTheme } = useTheme();

  useDismissable(ref, open, () => setOpen(false));

  // The shell only repaints once the stored mode came back.
  const mode = useSettingToggle("ui_mode", {
    errorLabel: "Couldn't switch modes",
    onSaved: () => {
      setOpen(false);
      navigate(paths.home);
    },
  });

  async function logout() {
    setError("");
    try {
      await post("/api/auth/logout");
    } catch (e) {
      // The menu stays open and says why — a silent failure here would leave
      // an operator believing they signed out when the session is still live.
      setError(errorText(e, "Sign-out failed"));
      return;
    }
    await qc.invalidateQueries({ queryKey: meQuery.queryKey });
    navigate(paths.login);
  }

  const item = "block w-full px-3 py-1.5 text-left text-sm text-ink hover:bg-fog-800 hover:text-ink disabled:opacity-50";

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen(!open)}
        aria-label="Account menu"
        aria-haspopup="menu"
        aria-expanded={open}
        className="flex h-8 w-8 items-center justify-center rounded-full border border-edge bg-fog-800 text-sm font-semibold text-ink transition-colors hover:border-edge-hover"
      >
        {handle.slice(0, 1).toUpperCase()}
      </button>
      {open && (
        <div
          role="menu"
          className="absolute right-0 top-10 z-30 w-52 overflow-hidden rounded-lg border border-veil-edge bg-fog-900 py-1 shadow-xl shadow-black/40"
        >
          <p className="border-b border-hair px-3 py-2 text-xs text-ink-muted">
            Signed in as <span className="font-medium text-ink">{handle}</span>
          </p>
          <div className="border-b border-hair px-3 py-2">
            <p className="pb-1.5 text-xs text-ink-muted">Appearance</p>
            <div
              role="radiogroup"
              aria-label="Appearance"
              className="flex gap-0.5 rounded-md bg-fog-800 p-0.5"
            >
              {(["system", "light", "dark"] as ThemePref[]).map((p) => (
                <button
                  key={p}
                  role="radio"
                  aria-checked={pref === p}
                  onClick={() => setTheme(p)}
                  className={`flex-1 rounded px-1.5 py-1 text-xs font-medium capitalize transition-colors ${
                    pref === p ? "bg-accent text-accent-on" : "text-ink-muted hover:text-ink"
                  }`}
                >
                  {p}
                </button>
              ))}
            </div>
            {/* The palettes live on their own page; basic mode has no Settings. */}
            {!basic && (
              <Link
                to={paths.settings("preferences/appearance")}
                onClick={() => setOpen(false)}
                className="mt-1.5 block text-xs text-ink-muted hover:text-ink"
              >
                Themes…
              </Link>
            )}
          </div>
          {/* No claim about the current mode until the settings arrive. */}
          {mode.value !== undefined && (
            <button
              role="menuitem"
              onClick={() => { setError(""); mode.set(basic ? "advanced" : "basic"); }}
              disabled={mode.isPending}
              className={item}
              title={
                basic
                  ? "Bring back the dashboard, inbox, and every setting"
                  : "Reduce fog to an app list with stop, restart, update, and open"
              }
            >
              {mode.isPending ? "Switching…" : basic ? "Switch to advanced mode" : "Switch to basic mode"}
            </button>
          )}
          <button onClick={logout} role="menuitem" className={item}>
            Sign out
          </button>
          {(error || mode.error) && <p className="px-3 py-1.5 text-xs text-warn">{error || mode.error}</p>}
        </div>
      )}
    </div>
  );
}
