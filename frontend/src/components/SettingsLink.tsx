// A link to a Settings page that spells its own name. Help text that says
// "see System → Network" goes stale the day the page moves; this reads the
// label from the Settings map, so the words follow the page.

import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { paths } from "../lib/paths";
import { settingsLabel, type SettingsTo } from "../lib/settings";

export default function SettingsLink({ to, anchor, className = "underline", onClick, children }: {
  to: SettingsTo;
  anchor?: string;
  className?: string;
  onClick?: () => void;
  // Overrides the spelled-out "Settings › Area › Page".
  children?: ReactNode;
}) {
  return (
    <Link to={paths.settings(to, anchor)} className={className} onClick={onClick}>
      {children ?? settingsLabel(to)}
    </Link>
  );
}
