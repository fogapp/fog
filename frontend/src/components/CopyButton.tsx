import { useEffect, useRef, useState } from "react";
import { buttonClass } from "./Button";

// "Copied ✓" is a claim — it shows only after a copy that actually worked.
// navigator.clipboard is undefined on plain-http origins (http://fog.lan),
// where the textarea/execCommand fallback does the job; if even that fails,
// the button says so instead of pretending.
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      // Off-screen but selectable — display:none would make the copy a no-op.
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand("copy");
      ta.remove();
      return ok;
    } catch {
      return false;
    }
  }
}

export default function CopyButton({ text }: { text: string }) {
  const [state, setState] = useState<"idle" | "ok" | "fail">("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  return (
    <button
      type="button"
      onClick={async () => {
        const ok = await copyText(text);
        setState(ok ? "ok" : "fail");
        clearTimeout(timer.current);
        timer.current = setTimeout(() => setState("idle"), 1500);
      }}
      className={buttonClass("secondary", "sm")}
    >
      {state === "ok" ? "Copied ✓" : state === "fail" ? "Copy failed" : "Copy"}
    </button>
  );
}
