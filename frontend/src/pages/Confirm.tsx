// "Confirm it's you": a signed-in person proves themselves again because an
// app asked — for a fresh sign-in, or for a passkey
// (docs/signin-contract.md). The proof lands on the session they already
// have, so no app they are
// signed in to is signed out by it.

import { useEffect, useState } from "react";
import { post } from "../api";
import Field from "../components/Field";
import Logo from "../components/Logo";
import { useAction } from "../lib/useAction";
import { followNext } from "../lib/next";
import { confirmWithPasskey, passkeysSupported } from "../lib/webauthn";

export default function Confirm() {
  const params = new URLSearchParams(window.location.search);
  const app = params.get("app") ?? "";
  const passkeyOnly = params.get("with") === "passkey";
  const [password, setPassword] = useState("");
  const [passkeys, setPasskeys] = useState(false);
  const { run, busy, error } = useAction();
  useEffect(() => setPasskeys(passkeysSupported()), []);

  function passkey() {
    run(async () => {
      await confirmWithPasskey();
      followNext();
    }, "The passkey wasn't accepted", { holdOnSuccess: true });
  }

  function submit(e: React.FormEvent) {
    e.preventDefault();
    run(async () => {
      await post("/api/people/reauth/password", { password });
      followNext();
    }, "That isn't your password", { holdOnSuccess: true });
  }

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <form onSubmit={submit} className="w-full max-w-sm space-y-4 rounded-xl border border-veil-edge bg-veil p-6">
        <h1 className="flex items-center justify-center gap-2 text-2xl font-semibold text-ink">
          <Logo className="h-9" />
          Confirm it's you
        </h1>
        <p className="text-center text-sm text-ink-muted">
          {passkeyOnly
            ? <>{app || "This app"} asks for your passkey.</>
            : <>{app || "This app"} asks you to sign in again.</>}
        </p>
        {passkeys ? (
          <button type="button" onClick={passkey} disabled={busy}
            className="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
            Use your passkey
          </button>
        ) : passkeyOnly && (
          <p className="text-sm text-warn">Passkeys need this page over https. Open fog at its domain to continue.</p>
        )}
        {!passkeyOnly && (
          <>
            <Field label="Password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus={!passkeys} />
            <button type="submit" disabled={busy}
              className={`w-full rounded-md py-2 text-sm font-semibold disabled:opacity-50 ${passkeys ? "border border-edge text-ink hover:bg-hover" : "bg-accent text-accent-on hover:bg-accent-hover"}`}>
              {busy ? "Confirming…" : "Confirm with password"}
            </button>
          </>
        )}
        {error && <p className="text-sm text-warn">{error}</p>}
      </form>
    </div>
  );
}
