// Making a room: a channel (open to the household), a group (invited), or a
// direct message. People come from the household directory; only those with
// a device that has published keys can be added right now, and the modal
// says so rather than failing later.

import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import type { ChatClient, DirectoryHousehold, DirectoryPerson } from "@fog/chat-sdk";
import Modal from "../../components/Modal";
import Field from "../../components/Field";
import { paths } from "../../lib/paths";
import { useAction } from "../../lib/useAction";
import SettingsLink from "../../components/SettingsLink";

export default function NewRoomModal({ client, kind, directory, onClose }: { client: ChatClient; kind: "channel" | "group" | "dm"; directory: DirectoryPerson[]; onClose: () => void }) {
  const navigate = useNavigate();
  const [label, setLabel] = useState("");
  const [topic, setTopic] = useState("");
  const [picked, setPicked] = useState<string[]>([]);
  const { run, busy, error } = useAction();
  const [people, setPeople] = useState(directory);
  const [households, setHouseholds] = useState<DirectoryHousehold[]>(client.households());
  const me = client.me();

  useEffect(() => {
    client.directory().then((p) => {
      setPeople(p);
      setHouseholds(client.households());
    }).catch(() => {});
  }, [client]);

  const candidates = people.filter((p) => p.handle !== me.handle);
  const addrOf = (handle: string) => `fog:${me.household.instanceId}/${handle}`;
  const canAdd = (p: DirectoryPerson) => p.devices.some((d) => d.keyPackages > 0);
  // A peer's directory carries no key package counts (those are fetched
  // when adding); anyone with a device is offered.
  const canAddRemote = (p: DirectoryPerson) => p.devices.length > 0;

  async function create() {
    await run(async () => {
      const room = await client.createRoom({
        kind,
        label: kind === "dm" ? undefined : label.trim(),
        topic: kind === "channel" ? topic.trim() : undefined,
        participants: picked,
      });
      onClose();
      navigate(paths.chat(room.id));
    }, "Couldn't create the room", { holdOnSuccess: true });
  }

  const title = kind === "channel" ? "New channel" : kind === "group" ? "New group" : "New direct message";
  const needLabel = kind !== "dm";
  const needPeople = kind === "dm";

  return (
    <Modal title={title} onClose={onClose}>
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (!busy) create();
        }}>
        {kind === "channel" && (
          <p className="text-hint text-ink-muted">
            Everyone in the household can see and join a channel. Its label is visible to the server; the messages never are.
          </p>
        )}
        {kind === "group" && <p className="text-hint text-ink-muted">A group is invitation-only. Only the people you add can see it exists.</p>}
        {kind === "dm" && <p className="text-hint text-ink-muted">A direct message has a fixed set of people. Two people always land in the same one.</p>}
        {needLabel && <Field label={kind === "channel" ? "Channel name" : "Group name"} value={label} onChange={(e) => setLabel(e.target.value)} autoFocus required maxLength={64} />}
        {kind === "channel" && <Field label="Topic" value={topic} onChange={(e) => setTopic(e.target.value)} maxLength={64} />}

        <div>
          <span className="field-label mb-1.5 block">{kind === "dm" ? "With" : "Add people now"}</span>
          {candidates.length === 0 && <p className="text-hint text-ink-muted">Nobody else has joined this household yet — invite them from <SettingsLink to="access/people" />.</p>}
          <ul className="max-h-56 space-y-0.5 overflow-y-auto">
            {candidates.map((p) => {
              const addr = addrOf(p.handle);
              const on = picked.includes(addr);
              const ok = canAdd(p);
              return (
                <li key={p.handle}>
                  <label className={`flex items-center gap-2 rounded-md px-2 py-1.5 text-cell ${ok ? "text-ink hover:bg-hover" : "text-ink-muted"}`}>
                    <input
                      type="checkbox"
                      checked={on}
                      disabled={!ok}
                      onChange={(e) => setPicked(e.target.checked ? [...picked, addr] : picked.filter((a) => a !== addr))}
                    />
                    <span className="flex-1">
                      {p.displayName || p.handle} <span className="font-mono text-xs text-ink-muted">@{p.handle}</span>
                      {p.role === "bot" && <span className="ml-1 text-xs text-ink-muted">· fog itself</span>}
                    </span>
                    {!ok && <span className="text-xs">no device online yet</span>}
                  </label>
                </li>
              );
            })}
          </ul>
          {households.map((h) => (
            <div key={h.instanceId} className="mt-3">
              <span className="field-label mb-1.5 block">{h.name}{h.online ? "" : " · offline"}</span>
              {h.people.length === 0 && <p className="text-hint text-ink-muted">They advertise nobody yet.</p>}
              <ul className="max-h-40 space-y-0.5 overflow-y-auto">
                {h.people.map((p) => {
                  const addr = `fog:${h.instanceId}/${p.handle}`;
                  const on = picked.includes(addr);
                  const ok = canAddRemote(p);
                  return (
                    <li key={addr}>
                      <label className={`flex items-center gap-2 rounded-md px-2 py-1.5 text-cell ${ok ? "text-ink hover:bg-hover" : "text-ink-muted"}`}>
                        <input
                          type="checkbox"
                          checked={on}
                          disabled={!ok}
                          onChange={(e) => setPicked(e.target.checked ? [...picked, addr] : picked.filter((a) => a !== addr))}
                        />
                        <span className="flex-1">
                          {p.displayName || p.handle} <span className="font-mono text-xs text-ink-muted">@{p.handle}</span>
                        </span>
                        {!ok && <span className="text-xs">no device yet</span>}
                      </label>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))}
          {households.length > 0 && kind === "channel" && (
            <p className="mt-2 text-hint text-ink-muted">People from other households can be in a channel, but only this household's people can join it on their own.</p>
          )}
        </div>

        {error && <p className="text-sm text-warn">{error}</p>}
        <div className="flex justify-end gap-2">
          <button type="button" onClick={onClose} className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-hover">
            Cancel
          </button>
          <button
            type="submit"
            disabled={busy || (needLabel && !label.trim()) || (needPeople && picked.length === 0)}
            className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
            {busy ? "Creating…" : "Create"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
