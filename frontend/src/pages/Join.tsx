// /join/<secret>: a person joining the household from an invite link. Public
// — the person has no account yet. Three steps: what the invite is, pick a
// handle and a way to sign in, done. Passkeys lead where the browser can do
// them (https + a domain); a password is the honest fallback elsewhere.

import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { ApiError, get, post, type JoinPreview, type PersonSignedIn } from "../api";
import Field from "../components/Field";
import Logo from "../components/Logo";
import { Loading } from "../components/Async";
import { passkeysSupported, registerPasskey } from "../lib/webauthn";

export default function Join() {
  const { secret = "" } = useParams();
  const [preview, setPreview] = useState<JoinPreview | null>(null);
  const [loadError, setLoadError] = useState("");
  const [handle, setHandle] = useState("");
  const [display, setDisplay] = useState("");
  const [password, setPassword] = useState("");
  const [usePassword, setUsePassword] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [joined, setJoined] = useState<{ handle: string } | null>(null);
  const [passkeyState, setPasskeyState] = useState<"idle" | "busy" | "done" | "skipped">("idle");

  useEffect(() => {
    get<JoinPreview>(`/api/join/${encodeURIComponent(secret)}`)
      .then((p) => {
        setPreview(p);
        setHandle(p.handle);
        setDisplay(p.displayName);
        setUsePassword(!(p.passkeysAvailable && passkeysSupported()));
      })
      .catch((e) => setLoadError(e instanceof ApiError ? e.message : "Couldn't read this invite"));
  }, [secret]);

  const canPasskey = !!preview?.passkeysAvailable && passkeysSupported();

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body: Record<string, string> = { handle, displayName: display };
      if (usePassword) body.password = password;
      const res = await post<PersonSignedIn>(`/api/join/${encodeURIComponent(secret)}`, body);
      // The join succeeded even when the new row couldn't be read back
      // (person: null); the invite is spent, so say welcome regardless.
      setJoined(res.person ?? { handle });
      if (!usePassword && canPasskey) {
        setPasskeyState("busy");
        try {
          await registerPasskey("This device");
          setPasskeyState("done");
        } catch (err) {
          setPasskeyState("idle");
          setError(err instanceof Error ? err.message : "The passkey wasn't created");
        }
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn't join");
    } finally {
      setBusy(false);
    }
  }

  async function addPasskeyLater() {
    setPasskeyState("busy");
    setError("");
    try {
      await registerPasskey("This device");
      setPasskeyState("done");
    } catch (err) {
      setPasskeyState("idle");
      setError(err instanceof Error ? err.message : "The passkey wasn't created");
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <div className="w-full max-w-sm space-y-5 rounded-xl border border-veil-edge bg-veil p-6">
        <h1 className="flex items-center justify-center gap-2 text-2xl font-semibold text-ink">
          <Logo className="h-9" />
          fog
        </h1>

        {loadError && (
          <div className="space-y-2 text-center">
            <p className="text-sm text-warn">{loadError}</p>
            <p className="text-xs text-ink-muted">Ask whoever runs this fog for a fresh link.</p>
          </div>
        )}

        {!loadError && !preview && <Loading label="Reading your invite…" />}

        {preview && !joined && (
          <form onSubmit={submit} className="space-y-4">
            <p className="text-sm text-ink-muted">
              You've been invited to join <span className="text-ink">{preview.household}</span>
              {preview.role === "guest" ? " as a guest" : ""}. Pick how you'll appear and how you'll sign in.
            </p>
            <Field
              label="Handle"
              value={handle}
              onChange={(e) => setHandle(e.target.value.toLowerCase())}
              disabled={!!preview.handle}
              hint={preview.handle ? "Chosen for you by the invite." : "Lowercase letters, digits, . _ or -. This is your address."}
              required
              autoFocus={!preview.handle}
            />
            <Field label="Display name" value={display} onChange={(e) => setDisplay(e.target.value)} placeholder={handle || "Your name"} />

            {canPasskey && (
              <div className="space-y-1 text-xs text-ink-muted">
                <label className="flex items-center gap-2 text-ink-muted">
                  <input type="radio" checked={!usePassword} onChange={() => setUsePassword(false)} />
                  Sign in with a passkey (Face ID, Touch ID, or your phone)
                </label>
                <label className="flex items-center gap-2 text-ink-muted">
                  <input type="radio" checked={usePassword} onChange={() => setUsePassword(true)} />
                  Sign in with a password
                </label>
              </div>
            )}
            {usePassword && (
              <Field
                label="Password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                hint={canPasskey ? undefined : "Passkeys need this fog to have a domain and https; a password works everywhere."}
              />
            )}
            {error && <p className="text-sm text-warn">{error}</p>}
            <button
              type="submit"
              disabled={busy || !handle}
              className="w-full rounded-md bg-accent py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
              {busy ? "Joining…" : "Join"}
            </button>
          </form>
        )}

        {joined && (
          <div className="space-y-3">
            <p className="text-sm text-ink">
              Welcome, <span className="font-medium">@{joined.handle}</span>. You're in.
            </p>
            {passkeyState === "busy" && <p className="text-xs text-ink-muted">Setting up your passkey…</p>}
            {passkeyState === "done" && <p className="text-xs text-ink-muted">Passkey saved. This device can sign you in.</p>}
            {passkeyState === "idle" && canPasskey && !usePassword && (
              <button onClick={addPasskeyLater} className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-fog-800">
                Add a passkey
              </button>
            )}
            {error && <p className="text-sm text-warn">{error}</p>}
            <p className="text-xs text-ink-muted">
              Chat opens here once your household turns it on. You can close this page.
            </p>
          </div>
        )}
      </div>
    </div>
  );
}
