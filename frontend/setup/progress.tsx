import { useEffect, useRef, useState } from "react";
import { get } from "../src/api";
import { Spinner } from "../src/components/Async";
import type { AgentState, Progress } from "./types";

// ProgressList renders a step stream (install or repair) with one overall
// bar. Used by both flows so they can never drift apart visually.
export function ProgressList({ steps, failed }: { steps: Map<string, Progress>; failed: string }) {
  const order = [...steps.values()].sort((a, b) => a.step - b.step);
  // Every event carries the step count; before the first one the bar just
  // sits at zero.
  const total = order[0]?.steps ?? 1;
  const completed = order.filter((p) => p.status === "done").length;
  const current = order.find((p) => p.status === "running");
  const overall = Math.round(((completed + (current ? current.percent / 100 : 0)) / total) * 100);

  return (
    <>
      <div className="h-2 overflow-hidden rounded-full bg-fog-800">
        <div
          className={`h-full rounded-full transition-all duration-500 ${failed ? "bg-warn" : "bg-fog-400"}`}
          style={{ width: `${overall}%` }}
        />
      </div>
      <ol className="space-y-2">
        {order.map((p) => (
          <li key={p.id} className="flex items-center gap-3 text-sm">
            {p.status === "done" && <span className="w-4 text-accent">✓</span>}
            {p.status === "running" && <Spinner size="h-3.5 w-3.5" />}
            {p.status === "failed" && <span className="w-4 text-warn">✕</span>}
            <span className={p.status === "done" ? "text-ink-muted" : "text-ink"}>
              {p.title}
              {p.status === "running" && p.percent > 0 && p.percent < 100 && (
                <span className="text-ink-muted"> — {p.percent}%</span>
              )}
              {p.detail && p.status !== "running" && (
                <span className="block text-xs text-ink-muted">{p.detail}</span>
              )}
            </span>
          </li>
        ))}
      </ol>
    </>
  );
}

// useProgress follows the agent's event stream (SSE, with a state poll as
// backstop) and reports phase changes.
export function useProgress(onPhase: (st: AgentState) => void) {
  const [steps, setSteps] = useState<Map<string, Progress>>(new Map());
  const [failed, setFailed] = useState("");
  const phaseRef = useRef("");

  useEffect(() => {
    const es = new EventSource("/api/setup/events");
    es.onmessage = (msg) => {
      const p = JSON.parse(msg.data) as Progress;
      setSteps((prev) => new Map(prev).set(p.id, p));
    };
    const poll = setInterval(async () => {
      try {
        const st = await get<AgentState>("/api/setup/state");
        if (st.phase === "failed") setFailed(st.error || "Something went wrong.");
        if ((st.phase === "done" || st.phase === "failed") && phaseRef.current !== st.phase) {
          phaseRef.current = st.phase;
          onPhase(st);
        }
      } catch {
        /* the agent may be briefly unreachable; keep trying */
      }
    }, 1500);
    return () => {
      es.close();
      clearInterval(poll);
    };
  }, [onPhase]);

  return { steps, failed };
}
