// Passkeys in the browser: the two WebAuthn ceremonies against fog's
// /api/people endpoints. go-webauthn hands us options with base64url fields;
// modern browsers parse that shape natively, older ones need the fields
// decoded by hand, so both paths are here.

import { post, type PasskeyCeremony } from "../api";

export function passkeysSupported(): boolean {
  return typeof window !== "undefined" && window.isSecureContext && !!window.PublicKeyCredential;
}

function b64uToBuf(s: string): ArrayBuffer {
  const pad = s.length % 4 === 0 ? "" : "=".repeat(4 - (s.length % 4));
  const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/") + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

function bufToB64u(b: ArrayBuffer): string {
  const bytes = new Uint8Array(b);
  let bin = "";
  for (const x of bytes) bin += String.fromCharCode(x);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function creationOptions(json: any): PublicKeyCredentialCreationOptions {
  const PKC = window.PublicKeyCredential as any;
  if (typeof PKC.parseCreationOptionsFromJSON === "function") return PKC.parseCreationOptionsFromJSON(json.publicKey);
  const pk = json.publicKey;
  return {
    ...pk,
    challenge: b64uToBuf(pk.challenge),
    user: { ...pk.user, id: b64uToBuf(pk.user.id) },
    excludeCredentials: (pk.excludeCredentials ?? []).map((c: any) => ({ ...c, id: b64uToBuf(c.id) })),
  };
}

function requestOptions(json: any): PublicKeyCredentialRequestOptions {
  const PKC = window.PublicKeyCredential as any;
  if (typeof PKC.parseRequestOptionsFromJSON === "function") return PKC.parseRequestOptionsFromJSON(json.publicKey);
  const pk = json.publicKey;
  return {
    ...pk,
    challenge: b64uToBuf(pk.challenge),
    allowCredentials: (pk.allowCredentials ?? []).map((c: any) => ({ ...c, id: b64uToBuf(c.id) })),
  };
}

function credentialToJSON(cred: PublicKeyCredential): unknown {
  const anyCred = cred as any;
  if (typeof anyCred.toJSON === "function") return anyCred.toJSON();
  const r = cred.response as any;
  const out: any = {
    id: cred.id,
    rawId: bufToB64u(cred.rawId),
    type: cred.type,
    clientExtensionResults: cred.getClientExtensionResults(),
    response: { clientDataJSON: bufToB64u(r.clientDataJSON) },
  };
  if (r.attestationObject) out.response.attestationObject = bufToB64u(r.attestationObject);
  if (r.authenticatorData) out.response.authenticatorData = bufToB64u(r.authenticatorData);
  if (r.signature) out.response.signature = bufToB64u(r.signature);
  if (r.userHandle) out.response.userHandle = bufToB64u(r.userHandle);
  if (typeof r.getTransports === "function") out.response.transports = r.getTransports();
  return out;
}

// registerPasskey adds a passkey to the signed-in person.
export async function registerPasskey(label: string): Promise<{ id: number; label: string }> {
  const begin = await post<PasskeyCeremony>("/api/people/me/passkeys/begin");
  const cred = (await navigator.credentials.create({ publicKey: creationOptions(begin.options) })) as PublicKeyCredential | null;
  if (!cred) throw new Error("No passkey was created");
  return post("/api/people/me/passkeys/finish", { ceremony: begin.ceremony, label, response: credentialToJSON(cred) });
}

// signInWithPasskey runs a discoverable-credential login.
// confirmWithPasskey proves, with one of the signed-in person's own
// passkeys, that they are still the one at the keyboard — recorded on the
// session they already have.
export async function confirmWithPasskey(): Promise<void> {
  const begin = await post<PasskeyCeremony>("/api/people/reauth/passkey/begin");
  const cred = (await navigator.credentials.get({ publicKey: requestOptions(begin.options) })) as PublicKeyCredential | null;
  if (!cred) throw new Error("No passkey was offered");
  await post("/api/people/reauth/passkey/finish", { ceremony: begin.ceremony, response: credentialToJSON(cred) });
}

export async function signInWithPasskey(): Promise<{ person: { handle: string } }> {
  const begin = await post<PasskeyCeremony>("/api/people/login/passkey/begin");
  const cred = (await navigator.credentials.get({ publicKey: requestOptions(begin.options) })) as PublicKeyCredential | null;
  if (!cred) throw new Error("No passkey was offered");
  return post("/api/people/login/passkey/finish", { ceremony: begin.ceremony, response: credentialToJSON(cred) });
}
