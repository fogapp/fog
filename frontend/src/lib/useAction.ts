// useAction owns one busy flag and one error line for a section's async
// user actions — a button's work that isn't a cached server write worth a
// useMutation. It replaces the setBusy(true) / setError("") / try / catch /
// finally block each such handler used to spell out. Several handlers may
// share it (password and passkey sign-in, say): they then share the flag
// and the line, as they did when each wrote both by hand.

import { useCallback, useState } from "react";
import { errorText } from "../components/Async";

interface RunOptions {
  // Stay busy after success: for an action that leaves the screen (a
  // sign-in, a dialog that closes), so the button can't be pressed twice
  // in the beat before it goes.
  holdOnSuccess?: boolean;
}

export function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  // run reports failure as error text and resolves undefined; it never throws.
  const run = useCallback(
    async <R>(work: () => Promise<R>, fallback: string, { holdOnSuccess = false }: RunOptions = {}): Promise<R | undefined> => {
      setBusy(true);
      setError("");
      try {
        const out = await work();
        if (!holdOnSuccess) setBusy(false);
        return out;
      } catch (e) {
        setError(errorText(e, fallback));
        setBusy(false);
        return undefined;
      }
    },
    [],
  );

  return { run, busy, error, setError };
}
