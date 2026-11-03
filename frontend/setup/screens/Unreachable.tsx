import { useEffect, useState } from "react";
import Button from "../../src/components/Button";
import { Spinner } from "../../src/components/Async";

export function UnreachableScreen({ retry }: { retry: () => Promise<boolean> }) {
  const [checking, setChecking] = useState(false);

  // Keep knocking — a restarting agent (its service auto-restarts) should
  // be picked back up without anyone touching anything.
  useEffect(() => {
    const t = setInterval(retry, 5000);
    return () => clearInterval(t);
  }, [retry]);

  return (
    <div className="space-y-4">
      <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">Can&apos;t reach the installer</h2>
      <p className="text-sm text-ink-muted">
        This page can&apos;t talk to the setup agent on the machine right now. If an install
        was running, it is still running there — nothing is decided by this page being
        disconnected.
      </p>
      <p className="text-xs text-ink-muted">
        Reconnecting automatically. If this persists: check the machine is still powered
        on and on the same network, then reload this page.
      </p>
      <Button
        variant="primary"
        className="w-full"
        disabled={checking}
        onClick={async () => {
          setChecking(true);
          await retry();
          setChecking(false);
        }}
      >
        {checking ? <Spinner /> : "Try now"}
      </Button>
    </div>
  );
}
