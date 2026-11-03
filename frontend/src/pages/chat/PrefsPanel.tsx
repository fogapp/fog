// This person's chat preferences: quiet hours, read receipts, link previews,
// an ntfy topic, and Web Push for this browser. Preferences live on the
// server because they steer what the server does (whether it wakes a phone);
// none of them touch what it can read.

import { useEffect, useState } from "react";
import { unb64, type ChatClient, type Prefs } from "@fog/chat-sdk";
import Modal from "../../components/Modal";
import { errorText } from "../../components/Async";
import { control } from "../../components/Field";
import { confirmFirst } from "../../lib/reauth";

const DEFAULTS: Prefs = {
  dnd: { enabled: false, start: "22:00", end: "07:00", tz: Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC" },
  receipts: true,
  previews: false,
  ntfyTopic: "",
  pushEnabled: false,
};

export default function PrefsPanel({ client, onClose }: { client: ChatClient; onClose: () => void }) {
  const [prefs, setPrefs] = useState<Prefs | null>(client.cachedPrefs());
  const [draft, setDraft] = useState<Prefs>(client.cachedPrefs() ?? DEFAULTS);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [saved, setSaved] = useState(false);
  // Push is a fact about this browser, read from it, not from the prefs.
  const [pushState, setPushState] = useState<"unsupported" | "checking" | "off" | "on" | "denied">("checking");

  useEffect(() => {
    client
      .prefs()
      .then((p) => {
        setPrefs(p);
        setDraft({ ...p, dnd: { ...p.dnd, tz: p.dnd.tz || DEFAULTS.dnd.tz } });
      })
      .catch((e) => setError(errorText(e, "Couldn't load preferences")));
  }, [client]);

  useEffect(() => {
    if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) {
      setPushState("unsupported");
      return;
    }
    if (Notification.permission === "denied") {
      setPushState("denied");
      return;
    }
    navigator.serviceWorker
      .getRegistration("/sw.js")
      .then((reg) => reg?.pushManager.getSubscription())
      .then((sub) => setPushState(sub ? "on" : "off"))
      .catch(() => setPushState("off"));
  }, []);

  const dirty = prefs && JSON.stringify(prefs) !== JSON.stringify(draft);

  async function save() {
    setBusy("save");
    setError("");
    setSaved(false);
    try {
      const p = await client.setPrefs(draft);
      setPrefs(p);
      setDraft(p);
      setSaved(true);
    } catch (e) {
      if (!confirmFirst(e, "Your chat alerts")) setError(errorText(e, "Couldn't save preferences"));
    } finally {
      setBusy("");
    }
  }

  async function enablePush() {
    setBusy("push");
    setError("");
    try {
      const perm = await Notification.requestPermission();
      if (perm !== "granted") {
        setPushState(perm === "denied" ? "denied" : "off");
        if (perm === "denied") setError("Notifications are blocked for this site in the browser.");
        return;
      }
      const reg = await navigator.serviceWorker.register("/sw.js");
      await navigator.serviceWorker.ready;
      const { publicKey } = await client.api.vapidKey();
      const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: unb64(publicKey) });
      const json = sub.toJSON();
      if (!json.endpoint || !json.keys?.p256dh || !json.keys?.auth) throw new Error("the browser returned an incomplete subscription");
      await client.api.subscribePush(client.me().deviceId, { endpoint: json.endpoint, keys: { p256dh: json.keys.p256dh, auth: json.keys.auth } });
      const p = await client.setPrefs({ ...draft, pushEnabled: true });
      setPrefs(p);
      setDraft(p);
      setPushState("on");
    } catch (e) {
      if (!confirmFirst(e, "Your chat alerts")) setError(errorText(e, "Couldn't turn on push"));
    } finally {
      setBusy("");
    }
  }

  async function disablePush() {
    setBusy("push");
    setError("");
    try {
      const reg = await navigator.serviceWorker.getRegistration("/sw.js");
      const sub = await reg?.pushManager.getSubscription();
      await sub?.unsubscribe();
      await client.api.unsubscribePush(client.me().deviceId);
      const p = await client.setPrefs({ ...draft, pushEnabled: false });
      setPrefs(p);
      setDraft(p);
      setPushState("off");
    } catch (e) {
      if (!confirmFirst(e, "Your chat alerts")) setError(errorText(e, "Couldn't turn off push"));
    } finally {
      setBusy("");
    }
  }

  const field = control;

  return (
    <Modal title="Chat preferences" onClose={onClose}>
      <div className="space-y-6">
        {!prefs && !error && <p className="text-hint text-ink-muted">Loading…</p>}

        <section className="space-y-2">
          <h3 className="subhead">Quiet hours</h3>
          <label className="flex items-center gap-2 text-cell text-ink">
            <input type="checkbox" checked={draft.dnd.enabled} onChange={(e) => setDraft({ ...draft, dnd: { ...draft.dnd, enabled: e.target.checked } })} />
            Hold notifications overnight
          </label>
          <div className={`flex flex-wrap items-center gap-2 text-cell text-ink-muted ${draft.dnd.enabled ? "" : "opacity-50"}`}>
            <span>from</span>
            <input type="time" value={draft.dnd.start} disabled={!draft.dnd.enabled} onChange={(e) => setDraft({ ...draft, dnd: { ...draft.dnd, start: e.target.value } })} className={field} aria-label="Quiet hours start" />
            <span>to</span>
            <input type="time" value={draft.dnd.end} disabled={!draft.dnd.enabled} onChange={(e) => setDraft({ ...draft, dnd: { ...draft.dnd, end: e.target.value } })} className={field} aria-label="Quiet hours end" />
            <input
              value={draft.dnd.tz}
              disabled={!draft.dnd.enabled}
              onChange={(e) => setDraft({ ...draft, dnd: { ...draft.dnd, tz: e.target.value } })}
              className={`${field} w-44`}
              aria-label="Time zone"
              placeholder="Europe/London"
              list="fog-tz"
            />
            <datalist id="fog-tz">
              {tzOptions().map((z) => (
                <option key={z} value={z} />
              ))}
            </datalist>
          </div>
          <p className="text-hint text-ink-muted">Pushes and ntfy stay quiet in the window. Messages still arrive; the badge still counts.</p>
        </section>

        <section className="space-y-2">
          <h3 className="subhead">Sharing</h3>
          <label className="flex items-start gap-2 text-cell text-ink">
            <input type="checkbox" className="mt-1" checked={draft.receipts} onChange={(e) => setDraft({ ...draft, receipts: e.target.checked })} />
            <span>
              Send read receipts
              <span className="block text-hint text-ink-muted">Travels sealed inside the room, so only members learn what you have read.</span>
            </span>
          </label>
          <label className="flex items-start gap-2 text-cell text-ink">
            <input type="checkbox" className="mt-1" checked={draft.previews} onChange={(e) => setDraft({ ...draft, previews: e.target.checked })} />
            <span>
              Fetch link previews
              <span className="block text-hint text-ink-muted">
                Fog fetches the page's title and image for you, then the card is sealed into the message. Fog sees the link you pasted; nothing else.
              </span>
            </span>
          </label>
        </section>

        <section className="space-y-2">
          <h3 className="subhead">ntfy</h3>
          <input
            value={draft.ntfyTopic}
            onChange={(e) => setDraft({ ...draft, ntfyTopic: e.target.value })}
            maxLength={64}
            placeholder="topic (optional)"
            aria-label="ntfy topic"
            className={`${field} w-full`}
          />
          <p className="text-hint text-ink-muted">
            A topic on this fog's ntfy server that only you subscribe to. The note says who wrote and where, never what.
          </p>
        </section>

        <div className="flex items-center gap-3">
          <button
            onClick={save}
            disabled={!dirty || busy === "save"}
            className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-40">
            {busy === "save" ? "Saving…" : "Save"}
          </button>
          {saved && !dirty && <span className="text-hint text-ink-muted">Saved.</span>}
        </div>

        <section className="space-y-2 border-t border-hair pt-4">
          <h3 className="subhead">Push to this browser</h3>
          {pushState === "unsupported" && <p className="text-hint text-ink-muted">This browser can't receive Web Push. On iOS, add fog to the Home Screen first.</p>}
          {pushState === "denied" && <p className="text-hint text-warn">Notifications are blocked for this site. Allow them in the browser's site settings, then come back.</p>}
          {pushState === "checking" && <p className="text-hint text-ink-muted">Checking…</p>}
          {(pushState === "off" || pushState === "on") && (
            <div className="flex items-center gap-3">
              <button
                onClick={pushState === "on" ? disablePush : enablePush}
                disabled={busy === "push"}
                className={`rounded-md border px-3 py-1.5 text-sm disabled:opacity-50 ${pushState === "on" ? "border-edge text-ink hover:bg-hover" : "border-accent/40 text-accent hover:bg-accent/10"}`}>
                {busy === "push" ? "Working…" : pushState === "on" ? "Turn off push here" : "Turn on push here"}
              </button>
              <span className="text-hint text-ink-muted">{pushState === "on" ? "This browser is subscribed." : "Not subscribed on this browser."}</span>
            </div>
          )}
          <p className="text-hint text-ink-muted">A push carries the room and the sender. The text stays sealed until you open chat.</p>
        </section>

        {error && <p className="text-hint text-warn">{error}</p>}
      </div>
    </Modal>
  );
}

function tzOptions(): string[] {
  try {
    const anyIntl = Intl as unknown as { supportedValuesOf?: (k: string) => string[] };
    return anyIntl.supportedValuesOf ? anyIntl.supportedValuesOf("timeZone") : [];
  } catch {
    return [];
  }
}
