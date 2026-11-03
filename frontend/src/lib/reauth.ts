// confirmFirst sends the person to confirm it's them when fog asks for a
// fresh proof before a credential changes (409 reauth_required), and back
// to this page after. Reports whether it did.
import { ApiError } from "../api";

export function confirmFirst(e: unknown, what = "This change"): boolean {
  // fog's own ApiError, or the chat SDK's (status and code of its own).
  const err = e as { status?: number; code?: string; body?: { code?: string } } | null;
  const asked = e instanceof ApiError || (err !== null && typeof err === "object" && "status" in err);
  if (asked && err?.status === 409 && (err.code === "reauth_required" || err.body?.code === "reauth_required")) {
    const back = window.location.pathname + window.location.search;
    window.location.href = `/confirm?${new URLSearchParams({ next: back, app: what })}`;
    return true;
  }
  return false;
}
