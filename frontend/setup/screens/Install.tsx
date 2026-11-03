import { useCallback } from "react";
import { post } from "../../src/api";
import Button from "../../src/components/Button";
import type { AgentState } from "../types";
import { ProgressList, useProgress } from "../progress";

export function InstallScreen({
  onDone,
  onRetry,
}: {
  onDone: (st: AgentState) => void;
  onRetry: () => void;
}) {
  const onPhase = useCallback(
    (st: AgentState) => {
      if (st.phase === "done") onDone(st);
    },
    [onDone],
  );
  const { steps, failed } = useProgress(onPhase);

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">
          {failed ? "The install hit a problem" : "Installing Fog"}
        </h2>
        {!failed && (
          <p className="mt-1 text-sm text-ink-muted">
            Sit back — this machine is doing the rest itself.
          </p>
        )}
      </div>
      <ProgressList steps={steps} failed={failed} />
      {failed && (
        <div className="space-y-3">
          <div className="rounded-lg border border-warn/30 bg-warn/10 p-4">
            <p className="text-sm text-warn">{failed}</p>
            <p className="mt-1 text-xs text-warn">
              Nothing on your other devices is affected.{" "}
              <a href="/api/setup/diagnostics" className="underline">
                Download the diagnostics log here
              </a>
              ; the installer also tries to save a copy to the USB stick&apos;s FOG-DATA
              partition, readable on any computer.
            </p>
          </div>
          <Button
            variant="primary"
            className="w-full"
            onClick={async () => {
              await post("/api/setup/retry");
              onRetry();
            }}
          >
            Try again
          </Button>
        </div>
      )}
    </div>
  );
}
