// Fog's one sign-in. There used to be two — an operator page and a member
// page, each asking a question only fog could answer, so the first thing a
// household saw was a fork in the road with no signpost. There is one door
// now: the credentials say who you are, and the role on your row decides
// what opens on the other side.

import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { get, post } from "../api";
import { meQuery } from "../queries";
import Field from "../components/Field";
import Logo from "../components/Logo";
import { passkeysSupported, signInWithPasskey } from "../lib/webauthn";
import { useAction } from "../lib/useAction";
import { followNext } from "../lib/next";

export default function Login() {
  const qc = useQueryClient();
  const params = new URLSearchParams(window.location.search);
  // The app that sent the person here, when one did — so the page can say
  // where signing in leads.
  const app = params.get("app") ?? "";
  const [handle, setHandle] = useState(params.get("hint") ?? "");
  const [password, setPassword] = useState("");
  const { run, busy, error } = useAction();
  const [passkeys, setPasskeys] = useState(false);

  // Households whose people this app lets in as visitors: signing
  // in with their own fog, not this one. Only on the way to an app.
  const next = params.get("next") ?? "";
  const [households, setHouseholds] = useState<{ id: number; name: string }[]>([]);
  useEffect(() => {
    if (!app || !next.startsWith("/oauth2/authorize?")) return;
    get<{ id: number; name: string }[]>(`/api/visit/peers?app=${encodeURIComponent(app)}`)
      .then(setHouseholds)
      .catch(() => setHouseholds([]));
  }, [app, next]);

  // The browser knows whether a passkey ceremony can run here (a secure
  // context); the server says the rest when the ceremony begins, and that
  // answer lands in the error line below.
  useEffect(() => setPasskeys(passkeysSupported()), []);

  // Signed in. Someone sent here by an app (fog's sign-in, or the gate)
  // goes back to it; everyone else re-reads who they are, and the shell for
  // their role mounts.
  async function done() {
    if (followNext()) return;
    await qc.invalidateQueries({ queryKey: meQuery.queryKey });
  }

  function submit(e: React.FormEvent) {
    e.preventDefault();
    run(async () => {
      await post("/api/auth/login", { handle, password });
      await done();
    }, "Sign-in failed", { holdOnSuccess: true });
  }

  function passkey() {
    run(async () => {
      await signInWithPasskey();
      await done();
    }, "The passkey wasn't accepted", { holdOnSuccess: true });
  }

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <form onSubmit={submit} className="w-full max-w-sm space-y-4 rounded-xl border border-veil-edge bg-veil p-6">
        <h1 className="flex items-center justify-center gap-2 text-2xl font-semibold text-ink">
          <Logo className="h-9" />
          fog
        </h1>
        {app && <p className="text-center text-sm text-ink-muted">Sign in to continue to <span className="text-ink">{app}</span></p>}
        {passkeys && (
          <button
            type="button"
            onClick={passkey}
            disabled={busy}
            className="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
            Sign in with a passkey
          </button>
        )}
        <Field label="Handle" value={handle} onChange={(e) => setHandle(e.target.value.toLowerCase())} autoFocus required />
        <Field label="Password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required />
        {error && <p className="text-sm text-warn">{error}</p>}
        <button
          type="submit"
          disabled={busy}
          className={`w-full rounded-md py-2 text-sm font-semibold disabled:opacity-50 ${passkeys ? "border border-edge text-ink hover:bg-hover" : "bg-accent text-accent-on hover:bg-accent-hover"}`}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
        {households.length > 0 && (
          <div className="space-y-2 border-t border-veil-edge pt-4">
            <p className="text-center text-sm text-ink-muted">From another household?</p>
            {households.map((h) => (
              <a
                key={h.id}
                href={`/visit/start?${new URLSearchParams({ peer: String(h.id), next })}`}
                className="block w-full rounded-md border border-edge py-2 text-center text-sm text-ink hover:bg-hover">
                Sign in with {h.name}'s fog
              </a>
            ))}
          </div>
        )}
      </form>
    </div>
  );
}
