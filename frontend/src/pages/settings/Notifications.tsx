// Settings › Preferences › Notifications: how fog interrupts you. ntfy
// carries the pushes — where they go is set here, and tested here — and the
// preferences below decide which events are worth one. The durable record,
// the Inbox, is never affected by anything on this page.

import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { post } from "../../api";
import { Loading, errorText } from "../../components/Async";
import Button from "../../components/Button";
import Field from "../../components/Field";
import { Section } from "../../components/Section";
import SettingRow, { SettingRows } from "../../components/SettingRow";
import Toggle from "../../components/Toggle";
import { useSettingToggle } from "../../lib/useSettingToggle";
import { SettingsPage } from "./Frame";
import { useSettingsForm } from "./useSettingsForm";

export default function Notifications() {
  return (
    <SettingsPage>
      <NtfySection />
      <WhenSection />
    </SettingsPage>
  );
}

function NtfySection() {
  const { value, set, SaveButton, loading } = useSettingsForm(["ntfy_server", "ntfy_topic"]);
  const [result, setResult] = useState("");
  // The test sends what is saved, not what is typed — a message that
  // arrives on an unsaved topic would prove the wrong thing.
  const test = useMutation({
    mutationFn: () => post("/api/notify/test"),
    onSuccess: () => setResult("Sent ✓ — check your ntfy app"),
    onError: (e) => setResult(errorText(e, "The test could not be sent")),
  });
  return (
    <Section
      id="ntfy"
      title="Where pushes go"
      description={
        <>
          Pushes go to a topic on an{" "}
          <a href="https://ntfy.sh" target="_blank" rel="noreferrer" className="underline">ntfy</a> server —
          the public one or your own. Subscribe to the topic in the ntfy app to receive them.
        </>
      }
    >
      <div className="max-w-lg space-y-4">
        {loading ? (
          <Loading label="Loading settings…" />
        ) : (
          <>
            <Field label="ntfy server" value={value("ntfy_server")} onChange={set("ntfy_server")} placeholder="https://ntfy.sh" />
            <Field label="Topic" value={value("ntfy_topic")} onChange={set("ntfy_topic")} placeholder="fog-basement" />
          </>
        )}
        {SaveButton}
        <div className="flex items-center gap-3 border-t border-hair pt-4">
          <Button onClick={() => { setResult(""); test.mutate(); }} disabled={test.isPending || loading}>
            {test.isPending ? "Sending…" : "Send a test notification"}
          </Button>
          {result && (
            <span className={`text-sm ${result.startsWith("Sent") ? "text-accent" : "text-warn"}`}>{result}</span>
          )}
        </div>
      </div>
    </Section>
  );
}

// The push preference for peer breakage. Applies on click: a preference
// should take effect when chosen, and the checkbox always reflects what the
// server stored.

// Which events push. Each applies on click: a preference should take
// effect when chosen, and the switch always reflects what the server
// stored. A notice turned off here still lands in the Inbox.
function WhenSection() {
  const peerPush = useSettingToggle("flare_peer_push", { errorLabel: "Couldn't save the push preference" });
  return (
    <Section id="when" title="Tell me when" description="Turning a push off still records the notice in the Inbox — it only stops the interruption.">
      <SettingRows>
        <SettingRow
          label="A peer's fleet breaks"
          hint="Push when a peer that sends its errors here reports one. Which peers send anything is decided on Connect."
          error={peerPush.error}
        >
          {peerPush.value !== undefined ? (
            <Toggle
              checked={peerPush.value !== "off"}
              disabled={peerPush.isPending}
              onChange={(on) => peerPush.set(on ? "" : "off")}
              aria-label="Push when a peer's fleet breaks"
            />
          ) : (
            <span className="text-hint text-ink-muted">{peerPush.settings.isError ? "unavailable" : "…"}</span>
          )}
        </SettingRow>
      </SettingRows>
    </Section>
  );
}
