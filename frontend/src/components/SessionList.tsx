// Signed-in sessions, as a person sees their own or the operator sees a
// household member's: which client, where from, how recently. Ending one
// signs that client out; the device and its keys stay.

import { timeAgo, uaSummary } from "../lib/format";

export interface SessionRow {
  id: number;
  deviceId?: number;
  deviceName?: string;
  clientKind: string;
  createdAt: string;
  expiresAt: string;
  lastSeenAt: string;
  userAgent?: string;
  remoteAddr?: string;
}

export default function SessionList({
  sessions, current, onEnd, busy,
}: {
  sessions: SessionRow[];
  current?: number;
  onEnd?: (id: number) => void;
  busy?: number;
}) {
  if (sessions.length === 0) return <p className="text-hint text-ink-muted">No live sessions.</p>;
  return (
    <ul className="divide-y divide-divider rounded-md border border-hair">
      {sessions.map((s) => {
        const isCurrent = current !== undefined && s.id === current;
        const ua = uaSummary(s.userAgent);
        return (
          <li key={s.id} className="flex items-center gap-3 px-3 py-2 text-cell">
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2 text-ink">
                <span className="truncate">{s.deviceName || (s.deviceId ? `Device ${s.deviceId}` : "Browser")}</span>
                <span className="rounded-full border border-hair px-2 py-px text-[11px] font-medium capitalize text-ink-muted">{s.clientKind}</span>
                {isCurrent && <span className="rounded-full bg-accent/10 px-2 py-px text-[11px] font-medium text-accent">This session</span>}
              </div>
              <div className="truncate text-xs text-ink-muted">
                seen {timeAgo(s.lastSeenAt)}
                {ua ? ` · ${ua}` : ""}
                {s.remoteAddr ? ` · ${s.remoteAddr}` : ""}
              </div>
            </div>
            {onEnd && (
              <button
                onClick={() => onEnd(s.id)}
                disabled={isCurrent || busy === s.id}
                title={isCurrent ? "You are using this session" : "Sign this client out"}
                className="rounded-md border border-warn/30 px-2 py-1 text-xs text-warn hover:bg-warn/10 disabled:cursor-not-allowed disabled:opacity-40">
                {busy === s.id ? "Ending…" : "End"}
              </button>
            )}
          </li>
        );
      })}
    </ul>
  );
}
