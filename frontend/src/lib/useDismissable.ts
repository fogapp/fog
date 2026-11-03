import { useEffect, type RefObject } from "react";

// Six dropdowns each grew their own "close when the operator clicks away"
// effect, and they had already drifted: some also closed on Escape, some
// didn't. One hook, both behaviors, for every popover.
//
// `active` gates the listeners so a closed menu costs nothing and a click
// that opens it isn't immediately counted as a click outside it.
export function useDismissable(
  ref: RefObject<HTMLElement | null>,
  active: boolean,
  onDismiss: () => void,
) {
  useEffect(() => {
    if (!active) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onDismiss();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onDismiss();
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [ref, active, onDismiss]);
}
