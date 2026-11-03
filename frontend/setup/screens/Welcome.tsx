import Button from "../../src/components/Button";

// What Fog is about, in three words — the first screen's whole pitch. The
// glyphs are drawn inline like every other icon in the frontend: a lock, a
// few linked nodes, a wrench.
const PILLARS: [string, string, React.ReactNode][] = [
  [
    "Private",
    "Nothing leaves it unless you say so.",
    <>
      <rect x="3" y="11" width="18" height="11" rx="2" />
      <path d="M7 11V7a5 5 0 0 1 10 0v4" />
    </>,
  ],
  [
    "Federated",
    "Connect to your friends, securely.",
    <>
      <circle cx="18" cy="5" r="3" />
      <circle cx="6" cy="12" r="3" />
      <circle cx="18" cy="19" r="3" />
      <path d="M8.59 13.51l6.83 3.98M15.41 6.51l-6.82 3.98" />
    </>,
  ],
  [
    "Managed",
    "Install, backup, and deploy automatically.",
    <path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z" />,
  ],
];

export function WelcomeScreen({
  onFresh,
  onRestore,
  onBack,
}: {
  onFresh: () => void;
  onRestore: () => void;
  onBack?: () => void;
}) {
  return (
    <div className="space-y-6 text-center">
      <div className="grid grid-cols-3 gap-6">
        {PILLARS.map(([title, blurb, glyph]) => (
          <div key={title}>
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.75"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
              className="mx-auto mb-2 h-7 w-7 text-ink-muted"
            >
              {glyph}
            </svg>
            <h2 className="text-sm font-bold text-ink">{title}</h2>
            <p className="mt-1 text-xs leading-relaxed text-ink-muted">{blurb}</p>
          </div>
        ))}
      </div>
      <div className="space-y-3">
        <Button variant="primary" className="w-full py-2.5" onClick={onFresh}>
          Set up a new Fog server
        </Button>
        <Button className="w-full py-2.5" onClick={onRestore}>
          Restore from a backup
        </Button>
        {onBack && (
          <Button variant="ghost" className="w-full" onClick={onBack}>
            Back
          </Button>
        )}
      </div>
    </div>
  );
}
