// The fleet error inbox (the Inbox's Errors tab): every open and regressed
// error group across every app — local and peer-relayed — newest first.
// Picking one opens it in the pane, whichever fleet it came from: the
// global endpoints reach every group by id, and a peer's app has no local
// page to send you to anyway. The owning app's own Errors tab is still one
// click away for the filters and siblings this list doesn't carry.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { get, type FleetErrorGroup } from "../../api";
import { LoadError, Loading, StaleNote, phaseOf } from "../../components/Async";
import { BulkBar, GroupBadges, GroupDetail, RowCheck, SelectAll, search } from "../app/Errors";
import { timeAgo } from "../../lib/format";
import { paths } from "../../lib/paths";
import { useFlash } from "../../lib/useFlash";
import { DetailPane, EmptyList, InboxRow, InboxSplit, NoSelection, Tag } from "./Split";
import { fleetErrorsKey } from "../../queries";
import { control } from "../../components/Field";

export default function Errors() {
  const [params, setParams] = useSearchParams();
  const [q, setQ] = useState("");
  const [sel, setSel] = useState<ReadonlySet<number>>(new Set());
  const [note, flash] = useFlash();

  const groups = useQuery({
    queryKey: fleetErrorsKey,
    queryFn: () => get<FleetErrorGroup[]>("/api/errors"),
    refetchInterval: 15000,
  });

  // Origin filter: "" = everything, "local" = this fleet, or a peer name.
  const origin = params.get("origin") ?? "";
  const origins = [...new Set((groups.data ?? []).map((g) => g.origin).filter(Boolean))] as string[];
  const byOrigin = (groups.data ?? []).filter((g) =>
    origin === "" ? true : origin === "local" ? !g.origin : g.origin === origin,
  );
  const visible = search(byOrigin, q, (g) => [g.app, g.title, g.culprit, g.errorClass, g.origin]);
  // Only rows still on screen can be acted on: a group filtered away, or
  // resolved out of the fleet list, drops out of the selection with it.
  const picked = visible.filter((g) => sel.has(g.id));
  const phase = phaseOf(groups, groups.data?.length);

  // ?g= stays the parameter it has always been: old deep links into a group
  // land on the same group, now beside the list instead of replacing it.
  const selectedId = params.get("g") ? Number(params.get("g")) : null;
  const current = visible.find((g) => g.id === selectedId) ?? null;
  const select = (id: number | null) => {
    const next = new URLSearchParams(params);
    if (id === null) next.delete("g");
    else next.set("g", String(id));
    setParams(next, { replace: true });
  };

  const setOrigin = (v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set("origin", v);
    else next.delete("origin");
    setParams(next, { replace: true });
  };

  return (
    <InboxSplit
      selected={selectedId !== null && !Number.isNaN(selectedId)}
      list={
        <>
          <div className="flex items-center gap-2 border-b border-divider px-4 py-2">
            {visible.length > 0 && <SelectAll rows={visible} sel={sel} onChange={setSel} />}
            <input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Search errors…"
              className={`${control} min-w-0 flex-1`}
            />
            {origins.length > 0 && (
              <select
                value={origin}
                onChange={(e) => setOrigin(e.target.value)}
                className={`${control} shrink-0`}
              >
                <option value="">All origins</option>
                <option value="local">This fleet</option>
                {origins.map((o) => (
                  <option key={o} value={o}>from {o}</option>
                ))}
              </select>
            )}
          </div>

          {picked.length > 0 && (
            <BulkBar
              ids={picked.map((g) => g.id)}
              // The global endpoints reach every group by id, peer-relayed
              // ones included — the per-app routes can't, since a peer's app
              // has no local namespace.
              urlFor={(id) => `/api/errors/${id}`}
              // This list is open and regressed groups only, so Reopen would
              // be a button that changes nothing.
              actions={["resolve", "ignore"]}
              onClear={() => setSel(new Set())}
              onResult={flash}
            />
          )}
          {note && <p className="px-4 py-2 text-xs text-ink-muted">{note}</p>}

          {phase === "loading" ? (
            <div className="p-4">
              <Loading label="Loading errors…" />
            </div>
          ) : phase === "error" ? (
            <div className="p-4">
              <LoadError label="Couldn't load errors." error={groups.error} onRetry={() => groups.refetch()} />
            </div>
          ) : phase === "empty" ? (
            <EmptyList
              title="Nothing broken anywhere. 🎉"
              hint="New errors and regressions land here the moment an app reports them."
            />
          ) : visible.length === 0 ? (
            <p className="p-6 text-center text-sm text-ink-muted">No errors match “{q.trim()}”.</p>
          ) : (
            visible.map((g) => (
              <InboxRow
                key={g.id}
                selected={g.id === selectedId}
                onClick={() => select(g.id)}
                title={g.title}
                when={timeAgo(g.lastSeen)}
                tag={g.origin ? <Tag tone="violet">{g.app} · from {g.origin}</Tag> : <Tag>{g.app}</Tag>}
                subtitle={`${g.culprit || g.errorClass} · ${g.eventsCount}×`}
                check={<RowCheck id={g.id} sel={sel} onChange={setSel} />}
              />
            ))
          )}
          {groups.isError && groups.data && (
            <div className="p-4">
              <StaleNote what="this list" />
            </div>
          )}
        </>
      }
      detail={
        selectedId !== null && !Number.isNaN(selectedId) ? (
          <DetailPane onBack={() => select(null)}>
            {/* Only a local group has an app page to open; a peer's app is
                not installed here, so there is nowhere to send you. */}
            {current && !current.origin && (
              <div className="mb-3 flex items-center gap-3">
                <GroupBadges group={current} />
                <Link
                  to={paths.app(current.app, "errors", { g: String(current.id) })}
                  className="text-xs text-ink-muted underline underline-offset-2 hover:text-ink"
                >
                  Open in {current.app} →
                </Link>
              </div>
            )}
            <GroupDetail base="/api/errors" id={selectedId} />
          </DetailPane>
        ) : (
          <NoSelection label="Pick an error to see its events and stack trace." />
        )
      }
    />
  );
}
