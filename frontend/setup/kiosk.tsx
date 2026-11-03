import { useCallback, useEffect, useRef, useState } from "react";
import { get, post } from "../src/api";
import Button from "../src/components/Button";
import { errorText } from "../src/components/Async";

// The kiosk on the machine's own display loads the wizard from localhost;
// every other surface is a browser somewhere on the LAN.
export const IS_KIOSK = ["localhost", "127.0.0.1"].includes(window.location.hostname);

interface KioskScale {
  scale: number;
  auto: number;
  manual: boolean;
}

const MIN_SCALE = 0.5;
const MAX_SCALE = 4;
const pct = (s: number) => `${Math.round(s * 100)}%`;

// Screen size, for the kiosk only: fog-kiosk sizes the wizard from the
// panel's resolution, and these overrule it when the guess is wrong. A change
// restarts the kiosk — the screen blanks and the page comes back at the new
// size — so the number shown is always the size actually being drawn, never
// the one just asked for.
export function ScaleControl() {
  const [st, setSt] = useState<KioskScale | null>(null);
  const [restarting, setRestarting] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const load = useCallback(async () => {
    try {
      setSt(await get<KioskScale>("/api/setup/display"));
    } catch {
      setSt(null); // no kiosk display to size
    }
  }, []);

  useEffect(() => {
    load();
    return () => clearTimeout(timer.current);
  }, [load]);

  if (!st) return null;

  const apply = async (scale: number) => {
    setBusy(true);
    setError("");
    try {
      const res = await post<{ restarting: boolean }>("/api/setup/display", { scale });
      if (res.restarting) {
        setRestarting(true);
        // The restart takes this page with it. Still here well after that
        // should have happened means it didn't.
        timer.current = setTimeout(async () => {
          setRestarting(false);
          setBusy(false);
          setError("The screen didn't restart to apply that size.");
          await load();
        }, 20_000);
        return;
      }
      await load();
    } catch (err) {
      setError(errorText(err, "Couldn't change the screen size."));
    }
    setBusy(false);
  };

  // Quarter steps from wherever it is now, even an off-step fog.scale.
  const step = (dir: 1 | -1) =>
    Math.min(MAX_SCALE, Math.max(MIN_SCALE, Math.round(st.scale * 4) / 4 + dir * 0.25));

  if (restarting) return <p>Resizing the screen…</p>;
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-center gap-2">
        <span>Screen size</span>
        <Button
          size="sm"
          className="w-7 px-0"
          aria-label="Smaller"
          disabled={busy || st.scale <= MIN_SCALE}
          onClick={() => apply(step(-1))}
        >
          −
        </Button>
        <span className="w-10 font-mono tabular-nums text-ink-muted">{pct(st.scale)}</span>
        <Button
          size="sm"
          className="w-7 px-0"
          aria-label="Larger"
          disabled={busy || st.scale >= MAX_SCALE}
          onClick={() => apply(step(1))}
        >
          +
        </Button>
        {st.manual && (
          <button
            className="underline hover:text-ink-muted disabled:opacity-40"
            disabled={busy}
            onClick={() => apply(0)}
          >
            Automatic ({pct(st.auto)})
          </button>
        )}
      </div>
      {error && <p className="text-warn">{error}</p>}
    </div>
  );
}
