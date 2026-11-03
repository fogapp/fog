// The Docker probe and one-click installer, shared by the first-run wizard
// (Setup) and Settings › Server › Docker. The wizard disappears after first run, so
// the Dependencies page must carry the exact same flow — one component, so the
// two doors can't drift apart.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { get, post, type DockerInstallStatus } from "../api";
import { Loading, errorText } from "./Async";
import { systemQuery } from "../queries";

export default function DockerInstall({
  buttonClassName = "rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover",
}: {
  buttonClassName?: string;
}) {
  // Fast only while waiting for Docker to appear: react-query polls at the
  // fastest interval among a query's observers, so a fixed 3s here would
  // drag the whole Docker page's 30s probe along with it.
  const system = useQuery({
    ...systemQuery,
    refetchInterval: (q) => (q.state.data?.docker.available ? false : 3000),
  });
  const install = useQuery({
    queryKey: ["docker-install"],
    queryFn: () => get<DockerInstallStatus>("/api/system/docker/install"),
    refetchInterval: (q) => (q.state.data?.running ? 1000 : false),
  });
  const [error, setError] = useState("");

  const available = system.data?.docker.available ?? false;
  // "Docker is not reachable" is a diagnosis; don't hand it out before the
  // first probe comes back, or this offers to install what's already there.
  const probing = !system.data && !system.isError;

  async function startInstall() {
    setError("");
    try {
      await post("/api/system/docker/install");
      install.refetch();
    } catch (err) {
      setError(errorText(err, "Install failed to start"));
    }
  }

  return probing ? (
    <Loading label="Looking for Docker on this server…" />
  ) : available ? (
    <p className="text-sm text-accent">
      ✓ Docker {system.data?.docker.version?.Version} is running.
    </p>
  ) : (
    <>
      <p className="text-sm text-ink-muted">
        {system.isError
          ? "Docker status is unavailable right now."
          : "Docker is not reachable. Fog can install it for you (Linux servers)."}
      </p>
      {/* Kept visible after a failure — the script's output is the
          diagnosis; a bare exit code isn't. (lines ?? []: older fogd
          serialized an empty log as null.) */}
      {(install.data?.running || install.data?.error) && (
        <pre className="max-h-40 overflow-y-auto rounded-md bg-fog-950 p-2 text-xs text-ink-muted">
          {(install.data.lines ?? []).slice(-15).join("\n") || "Starting…"}
        </pre>
      )}
      {install.data?.error && <p className="text-sm text-warn">{install.data.error}</p>}
      {error && <p className="text-sm text-warn">{error}</p>}
      {/* No install offer while the status itself is unreadable — pressing
          it would be acting on a state nobody has seen. */}
      {!system.isError && !install.data?.running && (
        <button onClick={startInstall} className={buttonClassName}>
          Install Docker
        </button>
      )}
    </>
  );
}
