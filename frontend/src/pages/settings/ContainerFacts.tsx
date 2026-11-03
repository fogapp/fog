// One line about a container fog supervises: whether it is up and since
// when, what it is using, what image it runs, how often Docker has had to
// restart it. The Postgres and Proxy sections both open on it, so the two
// read the same way, and a container that isn't there says so in the
// words its section supplies rather than a bare "not found".

import { useQuery } from "@tanstack/react-query";
import { get, type DependenciesStatus } from "../../api";
import { formatBytes, timeAgo } from "../../lib/format";

const dependenciesKey = ["dependencies"] as const;

export function useDependencies() {
  return useQuery({
    queryKey: dependenciesKey,
    queryFn: () => get<DependenciesStatus>("/api/system/dependencies"),
    refetchInterval: 15_000,
  });
}

export default function ContainerFacts({ which, absent }: { which: "postgres" | "proxy"; absent: string }) {
  const deps = useDependencies();
  const f = deps.data?.[which];
  if (!deps.data) {
    return <p className="text-hint text-ink-muted">{deps.isError ? "Container status unavailable." : "Reading the container…"}</p>;
  }
  if (!deps.data.docker.available) return <p className="text-hint text-warn">Docker is not reachable, so nothing can be said about the container.</p>;
  if (!f?.found) return <p className="text-hint text-ink-muted">{absent}</p>;
  const parts: string[] = [];
  if (f.running) {
    parts.push(f.startedAt ? `Up since ${timeAgo(f.startedAt)}` : "Up");
    if (f.memUsage !== undefined) parts.push(`${formatBytes(f.memUsage)} of memory`);
    if (f.cpuPercent !== undefined) parts.push(`${f.cpuPercent.toFixed(1)}% CPU`);
  } else {
    parts.push("Not running");
  }
  if (f.image) parts.push(f.image);
  parts.push(f.restartCount === 0 ? "never restarted by Docker" : `restarted by Docker ${f.restartCount} ${f.restartCount === 1 ? "time" : "times"}`);
  return (
    <p className="text-hint text-ink-muted">
      {parts.map((p, i) => (
        <span key={i}>
          {i > 0 && <span className="mx-1.5 text-ink-faint">·</span>}
          {p === f.image ? <span className="font-mono text-xs text-ink-muted">{p}</span> : <span className={f.running && i === 0 ? "text-accent" : !f.running && i === 0 ? "text-warn" : ""}>{p}</span>}
        </span>
      ))}
    </p>
  );
}
