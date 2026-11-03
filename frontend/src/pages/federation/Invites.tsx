// The Invites tab: the codes this fog has minted and not yet seen used,
// and the one paragraph of reassurance about how joining works.

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { del, type FedInvite } from "../../api";
import Button from "../../components/Button";
import { timeAgo, timeUntil } from "../../lib/format";
import { Group, Veil } from "../app/cockpit";
import { federationQuery } from "../../queries";

export function InvitesTab({ invites, onInvite, onAbout }: {
  invites: FedInvite[];
  onInvite: () => void;
  onAbout: () => void;
}) {
  const qc = useQueryClient();
  const revoke = useMutation({
    mutationFn: (id: number) => del(`/api/federation/invites/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: federationQuery.queryKey }),
  });

  return (
    <div>
      <Group title="Open invites" first right={<Button size="sm" variant="primary" onClick={onInvite}>Invite a peer</Button>}>
        {invites.length === 0 ? (
          <p className="text-hint text-ink-muted">No open invites. A code is single-use and expires after 24 hours.</p>
        ) : (
          <div className="space-y-2">
            {invites.map((inv) => (
              <Veil key={inv.id} className="flex flex-wrap items-center justify-between gap-4 px-[18px] py-4">
                <div>
                  <div className="text-cell text-ink">{grantSentence(inv.grants)}</div>
                  <div className="mt-[5px] text-hint text-ink-muted">
                    Expires {timeUntil(inv.expiresAt)} · one use · made {timeAgo(inv.createdAt)}
                  </div>
                </div>
                <span className="inline-flex items-center gap-2.5">
                  <span className="text-hint text-ink-muted">The code was shown once, when it was made</span>
                  <Button size="sm" variant="ghost" onClick={() => revoke.mutate(inv.id)} disabled={revoke.isPending}>Revoke</Button>
                </span>
              </Veil>
            ))}
          </div>
        )}
      </Group>

      <Group title="How joining works">
        <p className="max-w-[560px] text-sm leading-relaxed text-ink-muted">
          A code is a one-time handshake. The other fog enters it, the two exchange keys directly,
          and nothing about either machine passes through a third party.
        </p>
        <div className="mt-3">
          <Button size="sm" variant="ghost" onClick={onAbout}>More about Connect…</Button>
        </div>
      </Group>
    </div>
  );
}

// What an invite grants the fog that redeems it, as a sentence. The
// grants column is the JSON fogd stored when the code was minted.
function grantSentence(grants: string): string {
  try {
    const list = JSON.parse(grants) as { capability?: string }[];
    const caps = Array.isArray(list) ? list.map((g) => g.capability).filter(Boolean) : [];
    const sends = [caps.includes("flare.ingest") && "its errors", caps.includes("feedback.ingest") && "its feedback"].filter(Boolean);
    if (sends.length > 0) return `Lets the joining fog send ${sends.join(" and ")} here`;
    if (caps.length > 0) return `Grants ${caps.join(", ")}`;
  } catch {
    /* an older row */
  }
  return "Pairs the two fogs; nothing granted yet";
}
