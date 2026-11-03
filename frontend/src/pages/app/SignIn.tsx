// The Sign-in tab: how people sign in to this app through fog, who may,
// and in what role (docs/signin-contract.md). The policy is fog's;
// what a role lets someone do inside the app stays the app's.

import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { get, post, put } from "../../api";
import { LoadError, Loading, errorText } from "../../components/Async";
import Button from "../../components/Button";
import CopyButton from "../../components/CopyButton";
import { timeAgo } from "../../lib/format";
import { Group, Head, Kv, Pill, Row, Table, Veil } from "./cockpit";
import SettingsLink from "../../components/SettingsLink";
import { control } from "../../components/Field";
import { useConfirm } from "../../components/ConfirmDialog";

type Audience = "operator" | "household" | "people";

interface SigninView {
  mode: "oidc" | "gate" | "none";
  issuer: string;
  audience: Audience;
  people: { handle: string; role?: string }[];
  household: { handle: string; displayName: string; role: string }[];
  client: null | {
    clientId: string;
    redirectUris: string[];
    backchannelUri: string;
    scimBaseUrl: string;
    roles: string[];
    roleMap: Record<string, string>;
    requiredAcr: string;
    // The app is registered for the email scope.
    email: boolean;
    // A rotated secret is waiting for the app's next deploy.
    secretWaiting: boolean;
  };
  linked: { handle: string; firstAt: string; lastAt: string }[];
}

const signinKey = (name: string) => ["app", name, "signin"];

const selectCls = control;

const AUDIENCES: { value: Audience; label: string; hint: string }[] = [
  { value: "operator", label: "Only me", hint: "Nobody else can sign in." },
  { value: "household", label: "Household", hint: "Everyone in the household can sign in. Guests can't." },
  { value: "people", label: "Choose people", hint: "Only the people you pick, and you." },
];

export function SignInTab({ name }: { name: string }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: signinKey(name), queryFn: () => get<SigninView>(`/api/apps/${encodeURIComponent(name)}/signin`) });
  // Only the edits are local; until something is changed both read through
  // to the server's answer, so a refetch never resets a half-made choice.
  const [edits, setEdits] = useState<{ audience?: Audience; chosen?: Record<string, string | undefined> }>({});
  const [error, setError] = useState("");

  const stored = useMemo(
    () => Object.fromEntries((q.data?.people ?? []).map((p) => [p.handle, p.role ?? ""])) as Record<string, string | undefined>,
    [q.data],
  );
  const audience = edits.audience ?? q.data?.audience ?? "operator";
  const chosen = edits.chosen ?? stored;
  const setAudience = (a: Audience) => setEdits((e) => ({ ...e, audience: a }));
  const setChosen = (f: (c: Record<string, string | undefined>) => Record<string, string | undefined>) =>
    setEdits((e) => ({ ...e, chosen: f(e.chosen ?? stored) }));

  const save = useMutation({
    mutationFn: () => put(`/api/apps/${encodeURIComponent(name)}/signin`, {
      audience,
      people: Object.entries(chosen).filter(([, r]) => r !== undefined).map(([handle, role]) => ({ handle, role: role || undefined })),
    }),
    onSuccess: async () => {
      setError("");
      await qc.invalidateQueries({ queryKey: signinKey(name) });
      setEdits({});
    },
    onError: (e) => setError(errorText(e, "Couldn't save who can sign in")),
  });
  const signOut = useMutation({
    mutationFn: () => post<{ people: number }>(`/api/apps/${encodeURIComponent(name)}/signin/signout`),
    onSuccess: () => setError(""),
    onError: (e) => setError(errorText(e, "Couldn't sign everyone out")),
  });
  const [ask, dialog] = useConfirm();
  const rotate = useMutation({
    mutationFn: (now: boolean) => post<{ redeploying: boolean }>(`/api/apps/${encodeURIComponent(name)}/signin/rotate`, { now }),
    onSuccess: async () => { setError(""); await qc.invalidateQueries({ queryKey: signinKey(name) }); },
    onError: (e) => setError(errorText(e, "Couldn't rotate the secret")),
  });
  const revokeNow = async () => {
    if (await ask({
      title: `Revoke ${name}'s secret now?`,
      body: `The current secret stops working at once, and fog redeploys ${name} with a new one. Until that deploy finishes, nobody can sign in to ${name}. Do this when the secret may have leaked.`,
      action: "Revoke now",
      danger: true,
    })) rotate.mutate(true);
  };

  const view = q.data;
  const roles = view?.client?.roles ?? [];
  const impact = useMemo(() => view ? impactSentence(name, view, audience, chosen) : "", [name, view, audience, chosen]);
  const dirty = !!view && (audience !== view.audience ||
    JSON.stringify(Object.entries(chosen).filter(([, r]) => r !== undefined).sort()) !==
    JSON.stringify(view.people.map((p) => [p.handle, p.role ?? ""]).sort()));

  if (q.isLoading) return <Loading label="Loading sign-in…" />;
  if (q.isError || !view) return <LoadError label="Couldn't load sign-in" error={q.error} onRetry={() => q.refetch()} />;

  const members = view.household.filter((p) => p.role === "member");

  return (
    <div>
      <Group title="How people sign in" first>
        <Veil className="px-5 py-4">
          {view.mode === "oidc" && (
            <p className="text-cell text-ink">
              {name} signs people in with fog <Pill tone="ok">fog-signin/1</Pill>
              <span className="mt-1 block text-hint text-ink-muted">Each person uses their fog passkey. {name} learns who they are and the role you give them here.</span>
            </p>
          )}
          {view.mode === "gate" && (
            <p className="text-cell text-ink">
              fog's gate stands in front of {name}
              <span className="mt-1 block text-hint text-ink-muted">{name} can't sign people in by itself, so fog asks before every visit and tells it who passed.</span>
            </p>
          )}
          {view.mode === "none" && (
            <p className="text-cell text-ink">
              {name} signs people in by itself
              <span className="mt-1 block text-hint text-ink-muted">It doesn't declare fog sign-in and none of its routes are gated. Who can use it is up to its own accounts.</span>
            </p>
          )}
          {!view.issuer && view.mode !== "none" && (
            <p className="mt-2 text-hint text-warn">fog has no domain yet, so nobody can sign in through it. Set one under <SettingsLink to="network/domain" />.</p>
          )}
        </Veil>
      </Group>

      {view.mode !== "none" && (
        <Group title="Who can sign in" right={dirty ? <Button size="sm" variant="primary" disabled={save.isPending} onClick={() => save.mutate()}>Save</Button> : undefined}>
          <Veil className="space-y-4 px-5 py-4">
            <div className="flex flex-wrap gap-2" role="radiogroup" aria-label="Who can sign in">
              {AUDIENCES.map((a) => (
                <label key={a.value} className={`cursor-pointer rounded-md border px-3 py-2 text-cell ${audience === a.value ? "border-accent text-ink" : "border-edge text-ink-muted"}`}>
                  <input type="radio" name="audience" className="sr-only" checked={audience === a.value} onChange={() => setAudience(a.value)} />
                  {a.label}
                </label>
              ))}
            </div>
            <p className="text-hint text-ink-muted">{AUDIENCES.find((a) => a.value === audience)?.hint}</p>
            {members.length > 0 && (audience === "people" || roles.length > 0) && (
              <Table>
                <Head cols="minmax(0,1fr) 160px">
                  <span>Person</span><span>{roles.length > 0 ? "Role" : ""}</span>
                </Head>
                {members.map((p) => {
                  const picked = chosen[p.handle] !== undefined;
                  const included = audience === "household" || picked;
                  return (
                    <Row key={p.handle} cols="minmax(0,1fr) 160px">
                      <label className="inline-flex items-center gap-2 text-cell text-ink">
                        {audience === "people" && (
                          <input type="checkbox" checked={picked}
                            onChange={(e) => setChosen((c) => ({ ...c, [p.handle]: e.target.checked ? "" : undefined }))} />
                        )}
                        {p.displayName || p.handle} <span className="text-hint text-ink-muted">{p.handle}</span>
                      </label>
                      {roles.length > 0 && included ? (
                        <select className={selectCls} value={chosen[p.handle] ?? ""}
                          onChange={(e) => setChosen((c) => ({ ...c, [p.handle]: e.target.value }))}>
                          <option value="">{view.client?.roleMap.member ?? "member"} (default)</option>
                          {roles.map((r) => <option key={r} value={r}>{r}</option>)}
                        </select>
                      ) : <span />}
                    </Row>
                  );
                })}
              </Table>
            )}
            <p className="text-cell text-ink">{impact}</p>
            {error && <p className="text-hint text-warn">{error}</p>}
          </Veil>
        </Group>
      )}

      {view.mode === "oidc" && (
        <Group title="Signed in so far" right={view.linked.length > 0 && view.client?.backchannelUri
          ? <Button size="sm" disabled={signOut.isPending} onClick={() => signOut.mutate()}>Sign everyone out</Button>
          : undefined}>
          {signOut.isSuccess && (
            <p className="mb-3 text-hint text-ink-muted">fog is telling {name} that {signOut.data.people === 1 ? "1 person is" : `${signOut.data.people} people are`} signed out. They sign in again through fog.</p>
          )}
          {view.linked.length === 0 ? (
            <p className="text-hint text-ink-muted">Nobody has signed in to {name} through fog yet.</p>
          ) : (
            <Table>
              <Head cols="minmax(0,1fr) 160px 160px"><span>Person</span><span>First</span><span>Last</span></Head>
              {view.linked.map((l) => (
                <Row key={l.handle} cols="minmax(0,1fr) 160px 160px">
                  <span className="text-cell text-ink">{l.handle}</span>
                  <span className="text-hint text-ink-muted">{timeAgo(l.firstAt)}</span>
                  <span className="text-hint text-ink-muted">{timeAgo(l.lastAt)}</span>
                </Row>
              ))}
            </Table>
          )}
        </Group>
      )}

      {view.client && <VisitorsPart name={name} />}

      {view.client && (
        <Group title="Client" right={
          <span className="flex gap-2">
            <Button size="sm" disabled={rotate.isPending} onClick={() => rotate.mutate(false)}>Rotate secret</Button>
            <Button size="sm" variant="danger" disabled={rotate.isPending} onClick={revokeNow}>Revoke now…</Button>
          </span>
        }>
          <Veil className="px-5 py-4">
            <Kv label="Issuer" action={view.issuer ? <CopyButton text={view.issuer} /> : undefined}><span className="font-mono text-cell">{view.issuer || "—"}</span></Kv>
            <Kv label="Client id"><span className="font-mono text-cell">{view.client.clientId}</span></Kv>
            {view.client.redirectUris.map((u) => <Kv key={u} label="Redirect"><span className="font-mono text-cell">{u}</span></Kv>)}
            {view.client.requiredAcr && <Kv label="Requires"><span className="text-cell">a passkey</span></Kv>}
            {rotate.isSuccess && rotate.variables === true && (
              <p className="mt-2 text-hint text-ink-muted">
                The old secret no longer works. {rotate.data?.redeploying ? `${name} is redeploying with the new one; sign-in returns when it's done.` : `${name} can't sign anyone in until it is redeployed with the new one.`}
              </p>
            )}
            {view.client.secretWaiting && !(rotate.isSuccess && rotate.variables === true) && (
              <p className="mt-2 text-hint text-ink-muted">
                A new secret is waiting for {name}'s next deploy. The current one keeps working until {name} starts using the new one, and ten minutes after, so its old containers can finish.
              </p>
            )}
          </Veil>
        </Group>
      )}
      {dialog}
    </div>
  );
}

interface VisitorGrants {
  cap: string;
  roles: string[];
  grants: { peerId: number; name: string; role: string; homeAllows: boolean }[];
}

// Visitors from paired households (docs/signin-contract.md): only for
// an app that declares it hosts them, and only as far as its declared role.
// Their own fog signs them in; this says which households, and as what.
function VisitorsPart({ name }: { name: string }) {
  const qc = useQueryClient();
  const key = ["app", name, "signin", "visitors"];
  const q = useQuery({ queryKey: key, queryFn: () => get<VisitorGrants>(`/api/apps/${encodeURIComponent(name)}/signin/visitors`) });
  const [error, setError] = useState("");
  const save = useMutation({
    mutationFn: (grants: { peerId: number; role: string }[]) => put<VisitorGrants>(`/api/apps/${encodeURIComponent(name)}/signin/visitors`, { grants }),
    onSuccess: (data) => { setError(""); qc.setQueryData(key, data); },
    onError: (e) => setError(errorText(e, "Couldn't save visitors")),
  });
  const v = q.data;
  if (!v || !v.cap) return null;
  const set = (peerId: number, role: string) =>
    save.mutate(v.grants.map((g) => ({ peerId: g.peerId, role: g.peerId === peerId ? role : g.role })).filter((g) => g.role));
  return (
    <Group title="Visitors">
      <Veil className="space-y-3 px-5 py-4">
        <p className="text-hint text-ink-muted">
          People from a paired household sign in with their own fog. {name} lets a visitor be at most <span className="text-ink">{v.cap}</span>; they never appear among your people.
        </p>
        {v.grants.length === 0 ? (
          <p className="text-hint text-ink-muted">Pair with another household under Connect to open {name} to its people.</p>
        ) : (
          <Table>
            <Head cols="minmax(0,1fr) 160px"><span>Household</span><span>Visitors</span></Head>
            {v.grants.map((g) => (
              <Row key={g.peerId} cols="minmax(0,1fr) 160px">
                <span className="text-cell text-ink">
                  {g.name}
                  {g.role && !g.homeAllows && <span className="block text-hint text-ink-muted">Their fog doesn't let its people visit yet.</span>}
                </span>
                <select className={selectCls} value={g.role} disabled={save.isPending} onChange={(e) => set(g.peerId, e.target.value)}>
                  <option value="">Not allowed</option>
                  {v.roles.map((r) => <option key={r} value={r}>{r}</option>)}
                </select>
              </Row>
            ))}
          </Table>
        )}
        {error && <p className="text-hint text-warn">{error}</p>}
      </Veil>
    </Group>
  );
}

// impactSentence says, in the consent grammar's voice, what the choice on
// screen does — computed from the choice, so it changes as it does.
function impactSentence(app: string, view: SigninView, audience: Audience, chosen: Record<string, string | undefined>): string {
  const members = view.household.filter((p) => p.role === "member");
  const names = (audience === "household" ? members : members.filter((p) => chosen[p.handle] !== undefined))
    .map((p) => p.displayName || p.handle);
  const learns = view.mode !== "oidc" ? ""
    : view.client?.email
      ? ` ${app} will learn their handle, display name and, when they have added one, email address.`
      : ` ${app} will learn their handle and display name.`;
  if (audience === "operator" || names.length === 0) return `Only you can sign in to ${app}.`;
  const who = names.length <= 3 ? joinNames(names) : `${names.length} people`;
  const role = view.client?.roleMap.member;
  return `${who} will be able to sign in to ${app}${role ? ` as ${role}` : ""}.${learns}`;
}

function joinNames(n: string[]): string {
  if (n.length <= 1) return n.join("");
  return `${n.slice(0, -1).join(", ")} and ${n[n.length - 1]}`;
}
