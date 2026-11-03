// A link card. The sender fetched it (with fog's help, by choice) and sealed
// it into the message, so what is shown here is what they saw. The reader's
// browser fetches nothing: no image the card names is loaded (that would
// tell a third party the reader's address and when they read), and the link
// is followed only if it is http(s). The site named is the link's own host:
// a card's "site" is the sender's say, and could pass one site off as another.

import { linkHost, safeHref, type PreviewCard } from "@fog/chat-sdk";

export default function PreviewCardView({ card, onRemove }: { card: PreviewCard; onRemove?: () => void }) {
  const href = safeHref(card.url);
  const host = linkHost(card.url);
  return (
    <div className="mt-1.5 flex max-w-md items-stretch gap-3 rounded-md border border-hair bg-fog-950/60 p-2">
      <div className="min-w-0 flex-1">
        <div className="truncate text-hint text-ink-muted">{host || "not a web link"}</div>
        {href ? (
          <a href={href} target="_blank" rel="noopener noreferrer" className="block truncate text-cell font-medium text-ink hover:underline">
            {card.title || href}
          </a>
        ) : (
          <span className="block truncate text-cell font-medium text-ink">{card.title || card.url}</span>
        )}
        {card.description && <p className="line-clamp-2 text-hint text-ink-muted">{card.description}</p>}
      </div>
      {onRemove && (
        <button onClick={onRemove} className="self-start px-1 text-ink-muted hover:text-ink" title="Send without the preview" aria-label="Remove preview">
          ✕
        </button>
      )}
    </div>
  );
}
