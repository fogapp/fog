// Settings › People & access › People: the household directory. Who has an
// account here, how they sign in, how many devices they hold and how many
// of those they have vouched for, plus the invite links that bring new
// people in. The operator administers accounts; the operator cannot see
// into anyone's rooms, and this page says so where it matters (identity
// reset).
//
// A person is a row like any other managed thing: who they are and the
// facts under the name, their sessions one click away, and the rest of
// what can be done to an account behind the row's ⋯ — ten buttons on every
// row was a wall nobody read.
//
// At the foot, fog's signing keys and the emergency rotation: what the
// operator does when the key that signs everyone's sign-ins may have leaked.

import { useState, type MouseEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, get, patch, post, type PeopleDirectory, type PersonAdmin, type PersonInviteCreated } from "../../api";
import { LoadError, Loading, Spinner, errorText } from "../../components/Async";
import Button from "../../components/Button";
import { useConfirm } from "../../components/ConfirmDialog";
import ContextMenu, { type MenuItem } from "../../components/ContextMenu";
import CopyButton from "../../components/CopyButton";
import Field, { Select } from "../../components/Field";
import Modal from "../../components/Modal";
import { Section } from "../../components/Section";
import SessionList, { type SessionRow } from "../../components/SessionList";
import { RowItem, RowList } from "../../components/SettingRow";
import { SettingsPage } from "./Frame";
import { confirmFirst } from "../../lib/reauth";

const peopleKey = ["people"] as const;

export default function People() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: peopleKey, queryFn: () => get<PeopleDirectory>("/api/people"), refetchInterval: 30_000 });
  const [error, setError] = useState("");
  const [inviting, setInviting] = useState(false);
  const [ask, dialog] = useConfirm();
  const refresh = () => qc.invalidateQueries({ queryKey: peopleKey });

  const revoke = useMutation({
    mutationFn: (id: number) => del(`/api/people/invites/${id}`),
    onSuccess: () => { setError(""); return refresh(); },
    onError: (e) => setError(errorText(e, "Couldn't revoke")),
  });

  const people = q.data?.people ?? [];
  const invites = q.data?.invites ?? [];

  return (
    <SettingsPage
      description={
        <>
          Everyone with an account on this fog. Members can use chat and, later, apps you gate behind fog; only you
          administer the box. Nobody here — you included — can read a conversation they are not in.
        </>
      }
      actions={!inviting && <Button variant="primary" onClick={() => setInviting(true)}>Invite someone</Button>}
    >
      {inviting && <InviteForm onDone={() => setInviting(false)} onChange={refresh} />}

      <Section id="people" title="People" actions={q.data && <span className="text-hint text-ink-muted">{people.length}</span>}>
        {!q.data && !q.isError && <Loading label="Loading people…" />}
        {q.isError && <LoadError label="Couldn't load people." error={q.error} onRetry={() => q.refetch()} />}
        {people.length > 0 && (
          <RowList>
            {people.map((p) => (
              <PersonRow key={p.id} p={p} onChange={refresh} onError={setError} ask={ask} />
            ))}
          </RowList>
        )}
        {error && <p className="text-sm text-warn">{error}</p>}
      </Section>

      {invites.length > 0 && (
        <Section
          id="invites"
          title="Open invites"
          description="The link itself was shown once, when it was made. A revoked link stops working immediately."
        >
          <RowList>
            {invites.map((i) => (
              <RowItem
                key={i.id}
                title={<>{i.handleHint ? `@${i.handleHint}` : "Anyone with the link"}{i.displayHint && <span className="font-normal text-ink-muted"> · {i.displayHint}</span>}</>}
                meta={`${i.role} · expires ${new Date(i.expiresAt).toLocaleString()}`}
                actions={
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={revoke.isPending}
                    onClick={async () => {
                      const who = i.handleHint ? `@${i.handleHint}'s invite` : "this invite";
                      if (await ask({ title: `Revoke ${who}?`, body: "The link stops working immediately.", action: "Revoke", danger: true })) {
                        revoke.mutate(i.id);
                      }
                    }}
                  >
                    {revoke.isPending && revoke.variables === i.id ? "Revoking…" : "Revoke"}
                  </Button>
                }
              />
            ))}
          </RowList>
        </Section>
      )}
      <SigningKeys ask={ask} />
      {dialog}
    </SettingsPage>
  );
}

interface SigningKey {
  kid: string;
  signFrom: string;
  signUntil: string;
  unpublishAt: string;
  signing: boolean;
}

// The word the API asks for: its own confirm step, behind this page's.
const RETIRE_CONFIRM = "retire every key";

// SigningKeys is fog's signing keys and the emergency rotation
// (docs/signin-contract.md): every key retired at once, a new one signing
// immediately, and what the old ones signed no longer verifying anywhere.
function SigningKeys({ ask }: { ask: ReturnType<typeof useConfirm>[0] }) {
  const qc = useQueryClient();
  const key = ["signin", "keys"];
  const q = useQuery({ queryKey: key, queryFn: () => get<{ keys: SigningKey[] }>("/api/signin/keys") });
  const [error, setError] = useState("");
  const retire = useMutation({
    mutationFn: () => post<{ keys: SigningKey[] }>("/api/signin/keys/retire", { confirm: RETIRE_CONFIRM, signOutApps: true }),
    onSuccess: (data) => { setError(""); qc.setQueryData(key, data); },
    onError: (e) => setError(errorText(e, "Couldn't rotate the keys")),
  });
  const keys = q.data?.keys ?? [];
  return (
    <Section
      id="signing-keys"
      title="Signing keys"
      description="The keys fog signs sign-ins with, for apps and other households to check. They rotate on their own every 90 days."
      actions={
        <Button
          size="sm"
          variant="danger"
          disabled={retire.isPending}
          onClick={async () => {
            if (await ask({
              title: "Rotate every signing key now?",
              body: "Only if a key may have leaked. Every key is retired at once and a new one signs from now: anything the old keys signed stops being accepted, and every app is told everyone is signed out, so people sign in to their apps again. Their fog sessions stay.",
              action: "Rotate now",
              danger: true,
            })) retire.mutate();
          }}
        >
          {retire.isPending ? "Rotating…" : "Rotate now…"}
        </Button>
      }
    >
      {q.isError && <LoadError label="Couldn't load the signing keys." error={q.error} onRetry={() => q.refetch()} />}
      {keys.length > 0 && (
        <RowList>
          {keys.map((k) => (
            <RowItem
              key={k.kid}
              title={<span className="font-mono">{k.kid}</span>}
              meta={k.signing
                ? `signing until ${new Date(k.signUntil).toLocaleDateString()}`
                : new Date(k.signFrom) > new Date()
                  ? `published, signs from ${new Date(k.signFrom).toLocaleDateString()}`
                  : `retired, published until ${new Date(k.unpublishAt).toLocaleString()}`}
            />
          ))}
        </RowList>
      )}
      {retire.isSuccess && <p className="text-hint text-ink-muted">Every old key is retired, and fog refuses anything they signed. Apps built on fog-client refetch the key set within five minutes and refuse it too; every app has been told everyone is signed out.</p>}
      {error && <p className="text-sm text-warn">{error}</p>}
    </Section>
  );
}

// The invite form opens under the page header, the way every "add" on a
// Settings page does; the link it makes stays on screen, because it is
// shown exactly once.
function InviteForm({ onDone, onChange }: { onDone: () => void; onChange: () => void }) {
  const [handle, setHandle] = useState("");
  const [display, setDisplay] = useState("");
  const [role, setRole] = useState<"member" | "guest">("member");
  const [error, setError] = useState("");
  const [fresh, setFresh] = useState<{ url: string; expiresAt: string; handle: string } | null>(null);

  const create = useMutation({
    mutationFn: () => post<PersonInviteCreated>("/api/people/invites", { handle, displayName: display, role }),
    onSuccess: (r) => {
      setError("");
      setFresh({ url: r.url, expiresAt: r.expiresAt, handle });
      setHandle("");
      setDisplay("");
      onChange();
    },
    onError: (e) => setError(errorText(e, "Couldn't create the invite")),
  });

  if (fresh) {
    return (
      <div className="space-y-3 rounded-lg border border-accent/30 bg-accent/10 p-4 text-sm">
        <p className="text-accent">
          Send this link to {fresh.handle ? `@${fresh.handle}` : "the person"} now — it is shown once and expires{" "}
          {new Date(fresh.expiresAt).toLocaleDateString()}.
        </p>
        <p className="flex items-center gap-2 font-mono text-xs text-ink">
          <span className="min-w-0 break-all">{fresh.url}</span>
          <CopyButton text={fresh.url} />
        </p>
        <div className="flex gap-2">
          <Button size="sm" onClick={() => setFresh(null)}>Invite someone else</Button>
          <Button size="sm" variant="ghost" onClick={onDone}>Done</Button>
        </div>
      </div>
    );
  }

  return (
    <form
      className="space-y-4 rounded-lg border border-veil-edge bg-veil p-4"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate();
      }}
    >
      <p className="text-xs text-ink-muted">
        A link that lets one person join. Pin a handle if you want to choose their address; leave it blank and they
        pick. Links last a week and work once.
      </p>
      <div className="grid gap-4 sm:grid-cols-[1fr_1fr_10rem]">
        <Field label="Handle (optional)" value={handle} onChange={(e) => setHandle(e.target.value.toLowerCase())} maxLength={32} placeholder="sam" autoFocus />
        <Field label="Name (optional)" value={display} onChange={(e) => setDisplay(e.target.value)} maxLength={64} placeholder="Sam" />
        <Select label="Role" value={role} onChange={(e) => setRole(e.target.value as "member" | "guest")}>
          <option value="member">Member</option>
          <option value="guest">Guest</option>
        </Select>
      </div>
      <div className="flex items-center gap-2">
        <Button type="submit" variant="primary" disabled={create.isPending}>
          {create.isPending ? "Creating…" : "Create link"}
        </Button>
        <Button type="button" onClick={onDone}>Cancel</Button>
      </div>
      {error && <p className="text-sm text-warn">{error}</p>}
    </form>
  );
}

function PersonRow({ p, onChange, onError, ask }: {
  p: PersonAdmin;
  onChange: () => void;
  onError: (s: string) => void;
  ask: ReturnType<typeof useConfirm>[0];
}) {
  const [form, setForm] = useState<"password" | "email" | null>(null);
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const [password, setPassword] = useState("");
  const [email, setEmail] = useState(p.email ?? "");
  const [showSessions, setShowSessions] = useState(false);
  const [note, setNote] = useState("");
  const isOp = p.role === "operator";
  const sessionsQ = useQuery({
    queryKey: ["people", p.id, "sessions"],
    queryFn: () => get<{ sessions: SessionRow[] }>(`/api/people/${p.id}/sessions`),
    enabled: showSessions,
  });

  const act = async (fn: () => Promise<unknown>, fallback: string) => {
    try {
      await fn();
      onError("");
      setForm(null);
      setPassword("");
      onChange();
    } catch (e) {
      // A credential change asks for a fresh proof: confirm, then come back.
      if (!confirmFirst(e, "Changing someone's sign-in")) onError(errorText(e, fallback));
    }
  };

  const signOut = async () => {
    const ok = await ask({
      title: `Sign @${p.handle} out everywhere?`,
      body: (
        <p>
          Every browser and app signed in as them is signed out now — the lever for a lost phone when they cannot reach
          it themselves. Their devices and keys stay registered; they sign in again and pick up where they were.
        </p>
      ),
      action: "Sign out everywhere",
      danger: true,
    });
    if (ok) {
      await act(async () => {
        await post(`/api/people/${p.id}/signout`);
        setNote(`Signed @${p.handle} out of every session.`);
        if (showSessions) await sessionsQ.refetch();
      }, "Couldn't sign them out");
    }
  };
  const resetIdentity = async () => {
    const ok = await ask({
      title: `Reset @${p.handle}'s identity?`,
      body: (
        <>
          <p>
            For someone who lost every device and their recovery phrase. Their identity key is cleared and every device
            signed out; the next device they sign in on starts a new identity.
          </p>
          <p className="text-ink-muted">
            This recovers their <em>account</em>, not their conversations: fog holds no key that could bring those back,
            and every room they are in will show the reset and ask members to verify them again.
          </p>
        </>
      ),
      action: "Reset identity",
      danger: true,
    });
    if (ok) await act(() => post(`/api/people/${p.id}/reset-identity`), "Couldn't reset");
  };
  const remove = async () => {
    const ok = await ask({
      title: `Remove @${p.handle}?`,
      body: "Their account, devices, and sessions are deleted. Messages they sent stay in the rooms that hold them.",
      action: "Remove",
      danger: true,
    });
    if (ok) await act(() => del(`/api/people/${p.id}`), "Couldn't remove");
  };

  const items: MenuItem[] = [
    { label: "Set password…", onSelect: () => setForm("password") },
    { label: p.email ? "Change email…" : "Add email…", onSelect: () => setForm("email") },
    ...(!isOp
      ? [
          {
            label: p.status === "suspended" ? "Reactivate" : "Suspend",
            onSelect: () => void act(() => patch(`/api/people/${p.id}`, { status: p.status === "suspended" ? "active" : "suspended" }), "Couldn't change status"),
          },
          {
            label: p.role === "guest" ? "Make member" : "Make guest",
            onSelect: () => void act(() => patch(`/api/people/${p.id}`, { role: p.role === "guest" ? "member" : "guest" }), "Couldn't change role"),
          },
        ]
      : []),
    { separator: true },
    { label: "Sign out everywhere…", danger: true, onSelect: () => void signOut() },
    { label: "Reset identity…", danger: true, onSelect: () => void resetIdentity() },
    ...(!isOp ? [{ label: "Remove…", danger: true, onSelect: () => void remove() }] : []),
  ];

  const openMenu = (e: MouseEvent<HTMLButtonElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    setMenu({ x: r.right - 200, y: r.bottom + 4 });
  };

  return (
    <RowItem
      title={
        <span className="flex flex-wrap items-center gap-2">
          {p.displayName || p.handle}
          <span className="font-mono text-xs font-normal text-ink-muted">@{p.handle}</span>
          <span className="rounded-full border border-hair px-2 py-px text-[11px] font-medium capitalize text-ink-muted">{p.role}</span>
          {p.status === "suspended" && (
            <span className="rounded-full bg-warn/10 px-2 py-px text-[11px] font-medium text-warn">Suspended</span>
          )}
        </span>
      }
      meta={
        <>
          {p.credentials.length ? `Signs in with ${p.credentials.join(" or ")}` : "No way to sign in yet"} ·{" "}
          {p.devices === 0 ? "no devices" : `${p.devices} device${p.devices === 1 ? "" : "s"}, ${p.verifiedDevices} verified`}
          {p.hasIdentity ? "" : " · no identity key yet"}
          {p.lastSeenAt ? ` · seen ${new Date(p.lastSeenAt).toLocaleDateString()}` : ""}
          {p.email ? <> · <span className="font-mono">{p.email}</span> (unverified)</> : ""}
        </>
      }
      actions={
        <>
          <Button size="sm" variant="ghost" onClick={() => setShowSessions((v) => !v)} aria-expanded={showSessions}>
            {showSessions ? "Hide sessions" : "Sessions"}
          </Button>
          <Button size="sm" onClick={openMenu} aria-haspopup="menu" aria-label={`Manage @${p.handle}`} title="Manage">
            ⋯
          </Button>
        </>
      }
    >
      {note && <p className="text-xs text-ink-muted">{note}</p>}
      {showSessions && (
        <div className="space-y-1.5">
          <span className="subhead">Sessions</span>
          {sessionsQ.isLoading && <Spinner tone="border-fog-500" />}
          {sessionsQ.isError && <p className="text-xs text-warn">{errorText(sessionsQ.error, "Couldn't load sessions")}</p>}
          {sessionsQ.data && <SessionList sessions={sessionsQ.data.sessions} />}
        </div>
      )}
      {menu && <ContextMenu x={menu.x} y={menu.y} items={items} onClose={() => setMenu(null)} />}

      {form === "password" && (
        <Modal title={`Set a password for @${p.handle}`} onClose={() => setForm(null)}>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              void act(() => post(`/api/people/${p.id}/password`, { password }), "Couldn't set the password");
            }}
          >
            <p className="text-sm text-ink-muted">
              A temporary password they can sign in with and then change. Tell them in person; fog never shows it again.
            </p>
            <Field label="Password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoFocus required />
            <div className="flex justify-end gap-2">
              <Button type="button" variant="ghost" onClick={() => setForm(null)}>Cancel</Button>
              <Button type="submit" variant="primary">Set password</Button>
            </div>
          </form>
        </Modal>
      )}
      {form === "email" && (
        <Modal title={`Email for @${p.handle}`} onClose={() => setForm(null)}>
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              void act(() => patch(`/api/people/${p.id}`, { email }), "Couldn't save the email");
            }}
          >
            <p className="text-sm text-ink-muted">
              Optional. Some apps fog signs people in to want an address. It shows as unverified until fog can send mail,
              and fog never uses it to match anyone to an account, or shares it with another household.
            </p>
            <Field label="Email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoFocus />
            <div className="flex justify-end gap-2">
              <Button type="button" variant="ghost" onClick={() => setForm(null)}>Cancel</Button>
              <Button type="submit" variant="primary">Save</Button>
            </div>
          </form>
        </Modal>
      )}
    </RowItem>
  );
}
