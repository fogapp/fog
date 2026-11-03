// useJob follows one background job (a Drive move or index, an offsite
// fetch or recovery) until it settles. Every job page had its own copy of
// this: a query that polls while running, plus an effect that refreshes the
// page's lists once — and only once — when the job stops.

import { useEffect, useRef } from "react";
import { queryOptions, useQuery } from "@tanstack/react-query";
import { get } from "../api";

export type JobKind = "files" | "offsite";

interface JobLike {
  id: string;
  state: "running" | "done" | "failed" | "cancelled";
}

const jobURL: Record<JobKind, string> = {
  files: "/api/files/jobs/",
  offsite: "/api/offsite/jobs/",
};

function jobQuery<T extends JobLike>(kind: JobKind, id: string) {
  return queryOptions({
    queryKey: ["job", kind, id],
    queryFn: () => get<T>(jobURL[kind] + encodeURIComponent(id)),
  });
}

interface JobOptions<T> {
  // Poll period while the job runs; polling stops once it settles.
  interval?: number;
  // Runs once per job id, when the job is first seen settled.
  onDone?: (job: T) => void;
  // The job as the start request returned it, shown until the first poll.
  initial?: T;
}

export function useJob<T extends JobLike>(kind: JobKind, id: string | null | undefined, opts: JobOptions<T> = {}) {
  const { interval = 1000, initial } = opts;
  const q = useQuery({
    ...jobQuery<T>(kind, id ?? ""),
    enabled: Boolean(id),
    initialData: initial && initial.id === id ? initial : undefined,
    refetchInterval: (q) => (q.state.data?.state === "running" ? interval : false),
  });

  // The latest onDone, so a caller's inline closure needn't be memoised.
  const onDone = useRef(opts.onDone);
  useEffect(() => {
    onDone.current = opts.onDone;
  });
  const firedFor = useRef<string | null>(null);
  const settled = q.data && q.data.state !== "running" ? q.data : undefined;
  useEffect(() => {
    if (settled && id && firedFor.current !== id) {
      firedFor.current = id;
      onDone.current?.(settled);
    }
  }, [settled, id]);

  return q;
}
