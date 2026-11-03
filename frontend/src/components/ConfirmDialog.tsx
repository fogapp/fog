// Asking before a step that can't be taken back. The browser's confirm()
// is a grey box from another product, with "OK" for a button whether the
// step reboots the server or forgets a key; this is fog's own dialog, with
// the question as its title, the consequences under it, and a button that
// names the step. A destructive step's button is amber, never red —
// nothing in a house is an emergency.
//
//   const [ask, dialog] = useConfirm();
//   if (await ask({ title: "Reboot the server?", body: "…", action: "Reboot" })) …
//   return <>…{dialog}</>;

import { useRef, useState, type ReactNode } from "react";
import Button from "./Button";
import Modal from "./Modal";

export interface ConfirmRequest {
  // The question, as a title: "Remove share media?"
  title: string;
  // What happens, in a sentence or two.
  body?: ReactNode;
  // The step, as its button: "Remove", "Reboot", "Recover everything".
  action: string;
  // Amber button: the step deletes, stops or replaces something.
  danger?: boolean;
}

export function useConfirm(): [(req: ConfirmRequest) => Promise<boolean>, ReactNode] {
  const [req, setReq] = useState<ConfirmRequest | null>(null);
  const resolve = useRef<(ok: boolean) => void>(() => {});
  const ask = (r: ConfirmRequest) =>
    new Promise<boolean>((res) => {
      resolve.current = res;
      setReq(r);
    });
  const answer = (ok: boolean) => {
    setReq(null);
    resolve.current(ok);
  };
  const dialog = req && (
    <Modal title={req.title} onClose={() => answer(false)}>
      <div className="space-y-5">
        {req.body && <div className="space-y-2 text-sm text-ink-muted">{req.body}</div>}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => answer(false)}>Cancel</Button>
          {/* The step is the default: Enter takes it, as it did in the browser's box. */}
          <Button variant={req.danger ? "danger" : "primary"} onClick={() => answer(true)} autoFocus>
            {req.action}
          </Button>
        </div>
      </div>
    </Modal>
  );
  return [ask, dialog];
}
