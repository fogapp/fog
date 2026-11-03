import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { put, type Settings } from "../api";
import { errorText } from "../components/Async";
import { settingsQuery } from "../queries";

// A setting that applies on click — the Basic/Advanced switch, the peer-push
// checkbox — rather than sitting behind a form's Save button. Each lived in
// two places as its own copy of the same mutation, and two of those copies
// swallowed a failed write: the control snapped back with no word as to why.
//
// value is undefined until the settings arrive, so a caller never claims a
// state the server hasn't confirmed. set awaits the re-read before onSaved
// runs, so whatever the caller does next sees the stored value.
export function useSettingToggle<K extends keyof Settings>(
  key: K,
  opts: { errorLabel?: string; onSaved?: () => void } = {},
) {
  const qc = useQueryClient();
  const settings = useQuery(settingsQuery);
  const [error, setError] = useState("");

  const save = useMutation({
    mutationFn: (v: string) => put("/api/settings", { [key]: v }),
    onSuccess: async () => {
      setError("");
      await qc.invalidateQueries({ queryKey: settingsQuery.queryKey });
      opts.onSaved?.();
    },
    onError: (e) => setError(errorText(e, opts.errorLabel ?? "Couldn't save the setting")),
  });

  return {
    settings,
    value: settings.data?.[key] as string | undefined,
    set: (v: string) => save.mutate(v),
    isPending: save.isPending,
    error,
  };
}
