// Settings › Server › Activity log: the audit trail — who did what on this
// server, and when, newest first. The newest 200 entries load with the
// page and "Show older" reads back from there. The two narrowing controls
// work on what is loaded: who acted, and words in what they did.

import { useMemo, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { get, type AuditEntry } from "../../api";
import { LoadError, Loading } from "../../components/Async";
import Button from "../../components/Button";
import { FilterInput, Select } from "../../components/Field";
import { auditTone } from "../../lib/audit";
import { Dot } from "../app/cockpit";
import { SettingsPage } from "./Frame";

const PAGE = 200;

export default function Activity() {
  const audit = useInfiniteQuery({
    queryKey: ["audit"],
    queryFn: ({ pageParam }) => get<AuditEntry[]>(pageParam ? `/api/audit?before=${pageParam}` : "/api/audit"),
    initialPageParam: 0,
    // A short page is the end of the log.
    getNextPageParam: (last) => (last.length < PAGE ? undefined : last[last.length - 1].id),
  });
  const [who, setWho] = useState("");
  const [q, setQ] = useState("");

  const entries = useMemo(() => audit.data?.pages.flat() ?? [], [audit.data]);
  const actors = useMemo(() => [...new Set(entries.map((e) => e.actor))].sort(), [entries]);
  const needle = q.trim().toLowerCase();
  const rows = entries.filter(
    (e) => (!who || e.actor === who) && (!needle || e.action.toLowerCase().includes(needle) || e.subject.toLowerCase().includes(needle)),
  );

  return (
    <SettingsPage>
      <div className="space-y-4">
        <div className="flex flex-wrap items-center gap-3">
          <FilterInput value={q} onChange={setQ} label="Filter by action or subject" placeholder="Filter by action or subject…" />
          <Select value={who} onChange={(e) => setWho(e.target.value)} aria-label="Who acted">
            <option value="">Everyone</option>
            {actors.map((a) => (
              <option key={a} value={a}>{a}</option>
            ))}
          </Select>
          {audit.data && (
            <span className="ml-auto text-hint tabular-nums text-ink-muted">
              {rows.length === entries.length ? `${entries.length} entries` : `${rows.length} of ${entries.length} entries`}
            </span>
          )}
        </div>

        {audit.isPending && <Loading label="Loading activity…" />}
        {audit.isError && <LoadError label="Couldn't load the activity log." error={audit.error} onRetry={() => audit.refetch()} />}
        {audit.data && (
          <div className="overflow-x-auto rounded-lg border border-veil-edge bg-veil">
            <table className="w-full text-left text-cell">
              <thead>
                <tr className="caps-label border-b border-hair-strong">
                  <th className="px-4 py-2.5 font-medium">When</th>
                  <th className="px-3 py-2.5 font-medium">Who</th>
                  <th className="px-4 py-2.5 font-medium">What</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((e) => {
                  const tone = auditTone(e.action);
                  return (
                    <tr key={e.id} className="border-b border-divider last:border-0" title={e.detail || undefined}>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-hint text-ink-muted">{new Date(e.at).toLocaleString()}</td>
                      <td className="whitespace-nowrap px-3 py-2.5 text-ink">{e.actor}</td>
                      <td className="px-4 py-2.5">
                        <span className="flex items-center gap-2">
                          <Dot tone={tone === "warn" ? "warn" : tone === "ok" ? "ok" : "off"} />
                          <span className="font-mono text-hint text-ink">{e.action}</span>
                          {e.subject && <span className="min-w-0 truncate text-ink-muted">{e.subject}</span>}
                        </span>
                      </td>
                    </tr>
                  );
                })}
                {rows.length === 0 && (
                  <tr>
                    <td colSpan={3} className="px-4 py-4 text-ink-muted">
                      {entries.length === 0 ? "No activity yet." : "Nothing loaded matches — clear the filters, or show older entries."}
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        )}
        {audit.hasNextPage && (
          <Button onClick={() => audit.fetchNextPage()} disabled={audit.isFetchingNextPage}>
            {audit.isFetchingNextPage ? "Loading…" : "Show older"}
          </Button>
        )}
      </div>
    </SettingsPage>
  );
}
