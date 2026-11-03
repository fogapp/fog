// /chat/with/<person address>: the conversations you share with someone.
// Apps link here — Hub's "Message Dave" — without ever learning who talks to
// whom: the link is the app's, the list is fog's, and it is shown only to
// the person signed in (docs/signin-contract.md).

import { Link, useParams } from "react-router-dom";
import { LoadError, Loading } from "../../components/Async";
import { paths } from "../../lib/paths";
import { usePersonLookup } from "../../lib/personLookup";

export default function ChatWith() {
  const { address = "" } = useParams();
  const who = usePersonLookup({ address: decodeURIComponent(address) });

  if (who.isLoading) return <Loading label="Finding them…" className="p-8" />;
  if (who.isError || !who.data) {
    return <div className="p-8"><LoadError label="fog doesn't know who that is, or their household doesn't share them with yours" error={who.error} /></div>;
  }
  const p = who.data;
  const dm = p.sharedRooms.find((r) => r.kind === "dm");
  const rooms = p.sharedRooms.filter((r) => r.kind !== "dm");
  return (
    <div className="mx-auto max-w-lg space-y-8 px-4 py-10">
      <header>
        <h1 className="text-2xl text-ink">{p.displayName}</h1>
        <p className="text-hint text-ink-muted">
          <span className="font-mono">{p.handle}</span>
          {p.household ? <> · {p.household}'s household</> : null}
        </p>
      </header>

      <section className="space-y-2">
        <h2 className="subhead">Conversations</h2>
        {dm ? (
          <Link to={paths.chat(dm.id)} className="block rounded-md bg-accent px-4 py-2 text-center text-sm font-semibold text-accent-on hover:bg-accent-hover">
            Open your conversation with {p.displayName}
          </Link>
        ) : (
          <p className="text-hint text-ink-muted">
            You have no direct conversation with {p.displayName} yet. Start one from <Link className="text-accent" to={paths.chat()}>Chat</Link> › New room.
          </p>
        )}
        {rooms.length > 0 && (
          <ul className="divide-y divide-divider">
            {rooms.map((r) => (
              <li key={r.id}>
                <Link to={paths.chat(r.id)} className="block py-2 text-cell text-ink hover:text-ink">
                  {r.label || "Untitled room"} <span className="text-hint text-ink-muted">{r.kind}</span>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </section>

      {p.pages.length > 0 && (
        <section className="space-y-2">
          <h2 className="subhead">Elsewhere</h2>
          {p.pages.map((pg) => (
            <a key={pg.app} href={pg.url} className="block text-cell text-accent hover:underline" rel="noreferrer">{pg.label}</a>
          ))}
        </section>
      )}
    </div>
  );
}
