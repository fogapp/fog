// Settings › People & access › Your sign-in: your own passkeys and
// password, and the one place to add a passkey — the only other place that
// offers one is accepting an invite, which the operator who set the box up
// never did, so without it they could not use an app that asks for one.
// Below them, what recently happened to your sign-in: fog also tells you of
// each change through your chat notifications (push, ntfy).

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, type AccountEvent, type PersonCredential } from "../../api";
import { confirmFirst } from "../../lib/reauth";
import { LoadError, Loading, errorText, phaseOf } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import Field from "../../components/Field";
import { StatusLine } from "../../components/Section";
import SettingsLink from "../../components/SettingsLink";
import { passkeysSupported, registerPasskey } from "../../lib/webauthn";
import { SettingsPage } from "./Frame";
import { personMeQuery } from "../../queries";

// A guess at the device for a new passkey's label, editable before adding.
function deviceLabel(): string {
  const ua = navigator.userAgent;
  if (/iPhone/.test(ua)) return "iPhone";
  if (/iPad/.test(ua)) return "iPad";
  if (/Android/.test(ua)) return "Android";
  if (/Mac/.test(ua)) return "Mac";
  if (/Windows/.test(ua)) return "Windows PC";
  return "This device";
}

export default function SignIn() {
  const [ask, dialog] = useConfirm();
  const qc = useQueryClient();
  const me = useQuery(personMeQuery);
  const [label, setLabel] = useState("");
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");
  const add = useMutation({
    mutationFn: () => registerPasskey(label.trim() || deviceLabel()),
    onSuccess: () => { setAdding(false); setLabel(""); setError(""); return qc.invalidateQueries({ queryKey: personMeQuery.queryKey }); },
    onError: (e) => { if (!confirmFirst(e, "Your sign-in")) setError(errorText(e, "The passkey wasn't added")); },
  });
  const remove = useMutation({
    mutationFn: (id: number) => del(`/api/people/me/credentials/${id}`),
    onSuccess: () => { setError(""); return qc.invalidateQueries({ queryKey: personMeQuery.queryKey }); },
    onError: (e) => { if (!confirmFirst(e, "Your sign-in")) setError(errorText(e, "Could not remove it")); },
  });

  const creds: PersonCredential[] = me.data?.credentials ?? [];
  const canAdd = !!me.data?.passkeysAvailable && passkeysSupported();
  const phase = phaseOf(me, creds.length);

  return (
    <SettingsPage
      description={<>How you, <span className="font-mono">{me.data?.person.handle ?? "…"}</span>, sign in to fog — and to the apps that sign you in with fog. An app that asks for a passkey takes one of these.</>}
      actions={
        !adding && canAdd && (
          <Button onClick={() => { setAdding(true); setLabel(deviceLabel()); setError(""); }}>Add passkey</Button>
        )
      }
    >
      {phase === "loading" && <Loading label="Reading your sign-in…" />}
      {phase === "error" && <LoadError label="Couldn't read your sign-in." error={me.error} onRetry={() => me.refetch()} />}
      {me.data && !canAdd && (
        <StatusLine tone="idle">
          {me.data.passkeysAvailable
            ? "This browser can't make passkeys."
            : <>Passkeys need fog at its domain over https. Set one up under <SettingsLink to="network/domain" />, then open fog there to add a passkey.</>}
        </StatusLine>
      )}

      {creds.length > 0 && (
        <ul className="divide-y divide-divider/60 rounded-lg border border-veil-edge bg-veil">
          {creds.map((c) => (
            <li key={c.id} className="flex items-center gap-3 px-4 py-3">
              <div className="min-w-0 flex-1">
                <p className="text-sm font-medium text-ink">{c.kind === "passkey" ? `Passkey · ${c.label || "unnamed"}` : "Password"}</p>
                <p className="text-xs text-ink-muted">
                  Added {new Date(c.createdAt).toLocaleDateString()}
                  {c.lastUsedAt && <> · last used {new Date(c.lastUsedAt).toLocaleDateString()}</>}
                </p>
              </div>
              {creds.length > 1 && (
                <Button
                  size="sm"
                  variant="danger"
                  onClick={async () => {
                    const what = c.kind === "passkey" ? `the passkey "${c.label || "unnamed"}"` : "your password";
                    if (await ask({ title: `Remove ${what}?`, body: "You won't be able to sign in with it any more.", action: "Remove", danger: true })) remove.mutate(c.id);
                  }}
                  disabled={remove.isPending}
                >
                  {remove.isPending && remove.variables === c.id ? "Removing…" : "Remove"}
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}

      {adding && (
        <form
          className="space-y-4 rounded-lg border border-veil-edge bg-veil p-4"
          onSubmit={(e) => {
            e.preventDefault();
            add.mutate();
          }}
        >
          <div className="max-w-sm">
            <Field label="Name" autoFocus value={label} onChange={(e) => setLabel(e.target.value)} placeholder="the device it lives on"
              hint="Your browser or password manager asks where to keep it." />
          </div>
          <div className="flex items-center gap-3">
            <Button type="submit" variant="primary" disabled={add.isPending}>
              {add.isPending ? "Waiting for the passkey…" : "Add passkey"}
            </Button>
            <Button type="button" onClick={() => { setAdding(false); setLabel(""); setError(""); }}>Cancel</Button>
          </div>
        </form>
      )}

      {error && <p className="text-sm text-warn">{error}</p>}

      {(me.data?.activity?.length ?? 0) > 0 && (
        <section className="space-y-2">
          <h2 className="text-sm font-semibold text-ink">Recent changes</h2>
          <p className="text-xs text-ink-muted">
            If you don't recognise one, change your password and sign out the sessions you don't know. fog also tells you of each through your chat notifications, when you have them on.
          </p>
          <ul className="divide-y divide-divider/60 rounded-lg border border-veil-edge bg-veil">
            {me.data!.activity.map((e: AccountEvent, i: number) => (
              <li key={`${e.at}-${i}`} className="flex items-center gap-3 px-4 py-2">
                <p className="min-w-0 flex-1 text-sm text-ink">{e.summary}</p>
                <p className="shrink-0 text-xs text-ink-muted">{new Date(e.at).toLocaleString()}</p>
              </li>
            ))}
          </ul>
        </section>
      )}
      {dialog}
    </SettingsPage>
  );
}
