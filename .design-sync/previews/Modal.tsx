import { Button, Field, Modal } from "fog-frontend";

// A modal covers the whole viewport (fixed inset-0), so the card renders one
// open dialog (cardMode: single) over a page-sized fog surface: the h-96
// block is the page the dim covers.
function Page({ children }: { children: React.ReactNode }) {
  return (
    <div className="relative h-96 bg-fog-950 p-6 text-ink antialiased">
      <h1 className="text-xl font-semibold text-ink">Jellyfin</h1>
      <p className="mt-1 text-sm text-ink-muted">Media server · running · v10.10.3</p>
      {children}
    </div>
  );
}

export function ConfirmRemove() {
  return (
    <Page>
      <Modal title="Remove Jellyfin?" onClose={() => {}} width="max-w-md">
        <div className="space-y-4">
          <p className="text-sm text-ink-muted">
            The container and its config are removed. Media on the NAS stays where it is, and a
            backup from last night is still in the kit.
          </p>
          <div className="flex justify-end gap-2">
            <Button variant="ghost">Keep it</Button>
            <Button variant="danger">Remove app</Button>
          </div>
        </div>
      </Modal>
    </Page>
  );
}

export function WithForm() {
  return (
    <Page>
      <Modal title="Add a peer" onClose={() => {}}>
        <div className="space-y-4">
          <Field label="Peer address" placeholder="hub.lan" hint="The other fog's hostname on your network." />
          <Field label="Invite code" placeholder="fog-7QK2-…" />
          <div className="flex justify-end gap-2">
            <Button variant="ghost">Cancel</Button>
            <Button variant="primary">Connect</Button>
          </div>
        </div>
      </Modal>
    </Page>
  );
}
