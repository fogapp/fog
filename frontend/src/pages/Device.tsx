// /device: approve a sign-in on another screen (RFC 8628). The television
// shows a code; the person types it here (or arrives with it from the QR
// code) and sees which app and which device is asking before saying yes
// (docs/signin-contract.md).

import { useEffect, useState } from "react";
import { ApiError, post } from "../api";
import Field from "../components/Field";
import Logo from "../components/Logo";
import { useAction } from "../lib/useAction";

interface DeviceRequest {
  userCode: string;
  app: string;
  device: string;
}

export default function Device() {
  const initial = new URLSearchParams(window.location.search).get("user_code") ?? "";
  const [code, setCode] = useState(initial);
  const [request, setRequest] = useState<DeviceRequest | null>(null);
  const [done, setDone] = useState<"approved" | "denied" | null>(null);
  const { run, busy, error } = useAction();

  function look(c: string) {
    run(async () => {
      // A POST from this page: a GET a sibling host's page could fire would
      // spend this person's wrong-code budget.
      setRequest(await post<DeviceRequest>("/api/device/lookup", { userCode: c }));
    }, "That code isn't waiting for anyone");
  }
  // Arriving from the QR code: look it up straight away.
  useEffect(() => {
    if (initial) look(initial);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function decide(approve: boolean) {
    run(async () => {
      try {
        await post("/api/device/decide", { userCode: request!.userCode, approve });
        setDone(approve ? "approved" : "denied");
      } catch (e) {
        // The app asks for a passkey: confirm, then come back to this code.
        if (e instanceof ApiError && e.status === 409) {
          const back = `/device?user_code=${encodeURIComponent(request!.userCode)}`;
          window.location.href = `/confirm?with=passkey&app=${encodeURIComponent(request!.app)}&next=${encodeURIComponent(back)}`;
          return;
        }
        throw e;
      }
    }, "Couldn't record that");
  }

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <div className="w-full max-w-sm space-y-4 rounded-xl border border-veil-edge bg-veil p-6">
        <h1 className="flex items-center justify-center gap-2 text-2xl font-semibold text-ink">
          <Logo className="h-9" />
          Sign in a device
        </h1>
        {done === "approved" && <p className="text-center text-sm text-ink">Done. {request?.device || "The device"} is signing in to {request?.app}.</p>}
        {done === "denied" && <p className="text-center text-sm text-ink">Refused. Nothing was signed in.</p>}
        {!done && !request && (
          <form className="space-y-4" onSubmit={(e) => { e.preventDefault(); look(code); }}>
            <p className="text-center text-sm text-ink-muted">Type the code your screen shows.</p>
            <Field label="Code" value={code} onChange={(e) => setCode(e.target.value.toUpperCase())} autoFocus autoComplete="off" required />
            <button type="submit" disabled={busy}
              className="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
              Continue
            </button>
          </form>
        )}
        {!done && request && (
          <div className="space-y-4">
            <p className="text-center text-sm text-ink">
              <span className="text-ink">{request.app}</span> on <span className="text-ink">{request.device || "a device"}</span> wants to sign in as you.
            </p>
            <p className="text-center font-mono text-sm text-ink-muted">{request.userCode}</p>
            <p className="text-center text-hint text-ink-muted">Only say yes if you are looking at that screen and it shows this code.</p>
            <div className="flex gap-2">
              <button onClick={() => decide(false)} disabled={busy}
                className="flex-1 rounded-md border border-edge py-2 text-sm text-ink hover:bg-hover disabled:opacity-50">
                No
              </button>
              <button onClick={() => decide(true)} disabled={busy}
                className="flex-1 rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
                Sign it in
              </button>
            </div>
          </div>
        )}
        {error && <p className="text-sm text-warn">{error}</p>}
      </div>
    </div>
  );
}
