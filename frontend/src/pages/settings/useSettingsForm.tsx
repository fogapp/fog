// Shared save plumbing for every settings tab: load the current values, track
// edits, and save only what changed. Returns JSX (the Save button), which is
// why this is a .tsx and not a .ts.
//
// Only the edits are local state; every other value reads straight through
// to the settings query. Copying the server's values into the form instead
// meant any refetch of ["settings"] — the mode switch, another section's
// Save — reset every mounted form and threw away what was being typed.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { put, type Settings as SettingsT } from "../../api";
import { LoadError, errorText } from "../../components/Async";
import Button from "../../components/Button";
import { useFlash } from "../../lib/useFlash";
import { settingsQuery } from "../../queries";

export function useSettingsForm<K extends keyof SettingsT>(keys: K[]) {
  const qc = useQueryClient();
  const settings = useQuery(settingsQuery);
  const [edits, setEdits] = useState<Partial<Pick<SettingsT, K>>>({});
  const [saved, flashSaved] = useFlash(2000);
  const [error, setError] = useState("");

  const save = useMutation({
    // Only this section's touched keys: an untouched field never writes back
    // whatever the form last saw over a value changed since.
    mutationFn: (body: Partial<Pick<SettingsT, K>>) => put("/api/settings", body),
    // Awaited so "Saving…" covers the write *and* the re-read; the tick only
    // appears once the stored settings have actually come back.
    onSuccess: async (_, body) => {
      await qc.invalidateQueries({ queryKey: settingsQuery.queryKey });
      // Drop what was saved, keeping anything typed while the save was out.
      setEdits((cur) => {
        const next = { ...cur };
        for (const k of Object.keys(body) as K[]) if (next[k] === body[k]) delete next[k];
        return next;
      });
      setError("");
      flashSaved("Saved ✓");
    },
    onError: (e) => setError(errorText(e, "Save failed")),
  });

  const submit = () => {
    const body: Partial<Pick<SettingsT, K>> = {};
    for (const k of keys) if (edits[k] !== undefined) body[k] = edits[k];
    save.mutate(body);
  };

  // undefined drops the edit, so the field reads the stored value again.
  const setValue = (k: K, v: string | undefined) =>
    setEdits((cur) => {
      const next = { ...cur };
      if (v === undefined) delete next[k];
      else next[k] = v as SettingsT[K];
      return next;
    });
  const set = (k: K) => (e: React.ChangeEvent<HTMLInputElement>) => setValue(k, e.target.value);
  const value = (k: K) => (edits[k] as string | undefined) ?? (settings.data?.[k] as string | undefined) ?? "";

  // Empty inputs before the settings land read as "nothing configured", and
  // saving from that state would post blanks over real values. The form waits.
  const loading = !settings.data && !settings.isError;

  const SaveButton = (
    <div className="space-y-2">
      {settings.isError && !settings.data && (
        <LoadError label="Couldn't load your current settings." error={settings.error} onRetry={() => settings.refetch()} />
      )}
      {error && <p className="text-sm text-warn">{error}</p>}
      <Button variant="primary" onClick={submit} disabled={save.isPending || loading || !settings.data}>
        {saved || (save.isPending ? "Saving…" : "Save")}
      </Button>
    </div>
  );
  return { value, set, setValue, SaveButton, loading };
}
