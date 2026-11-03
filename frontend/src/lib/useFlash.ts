import { useCallback, useEffect, useRef, useState } from "react";

// Seven copies of "set a message, clear it on a timer" had accumulated, none
// of which cancelled the timer on unmount — navigating away mid-flash left a
// setState pointing at a gone component, and a second flash before the first
// expired had its timer clobbered so the message lingered.

export function useFlash(ms = 4000): [string, (message: string) => void] {
  const [message, setMessage] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const clear = useCallback(() => {
    if (timer.current !== undefined) {
      clearTimeout(timer.current);
      timer.current = undefined;
    }
  }, []);

  const flash = useCallback(
    (next: string) => {
      clear(); // a new message restarts the clock rather than inheriting it
      setMessage(next);
      if (next) timer.current = setTimeout(() => setMessage(""), ms);
    },
    [clear, ms],
  );

  useEffect(() => clear, [clear]);

  return [message, flash];
}
