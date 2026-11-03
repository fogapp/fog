// The page was opened over plain http from another machine, so the browser
// withholds WebCrypto and Chat cannot start. Say why, and point at the https
// address fog already has when a domain is set up.
import { useEffect, useState } from "react";
import SettingsLink from "../../components/SettingsLink";

interface MeLite {
  operator?: boolean;
  household?: { domain?: string };
}

export default function InsecurePanel() {
  const [me, setMe] = useState<MeLite | null>(null);
  useEffect(() => {
    let alive = true;
    fetch("/api/chat/me", { credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => alive && setMe(j))
      .catch(() => alive && setMe(null));
    return () => {
      alive = false;
    };
  }, []);
  const domain = me?.household?.domain ?? "";
  const httpsUrl = domain ? `https://fog.${domain}/chat` : "";
  const here = typeof window !== "undefined" ? window.location.host : "";

  return (
    <div className="flex h-[calc(100vh-3.5rem)] flex-col items-center justify-center gap-3 px-6 text-center">
      <p className="text-sm text-warn">Chat needs a secure connection.</p>
      <p className="max-w-md text-hint text-ink-muted">
        You opened fog at <span className="font-mono">{here}</span> over plain http. Browsers only unlock their cryptography on https:// or on
        localhost, and Chat encrypts everything on this device before it leaves, so it cannot start here.
      </p>
      {httpsUrl ? (
        <a href={httpsUrl} className="rounded border border-fog-600 px-3 py-1.5 text-sm text-ink hover:bg-fog-800">
          Open {httpsUrl.replace(/^https:\/\//, "").replace(/\/chat$/, "")} instead
        </a>
      ) : me?.operator ? (
        <p className="max-w-md text-hint text-ink-muted">
          Set up a domain in <SettingsLink to="network/domain" />: fog then answers at https://fog.&lt;your domain&gt; with a real certificate, and Chat works from every
          device in the house.
        </p>
      ) : me ? (
        <p className="max-w-md text-hint text-ink-muted">Ask your fog's operator for its https address; until a domain is set up there is none.</p>
      ) : null}
    </div>
  );
}
