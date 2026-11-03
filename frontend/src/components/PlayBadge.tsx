// The play mark laid over a video's poster frame: a dark disc so it reads
// on any frame, a white triangle nudged right of centre so it looks
// centred. Drawn inline like FileIcon, so the page carries no icon library.

export default function PlayBadge({ className = "h-8 w-8" }: { className?: string }) {
  return (
    <svg viewBox="0 0 40 40" className={`${className} shrink-0 drop-shadow-md transition-transform group-hover:scale-105`} aria-hidden>
      <circle cx="20" cy="20" r="19" fill="rgba(0,0,0,0.55)" stroke="rgba(255,255,255,0.7)" strokeWidth="1.5" />
      <path d="M16 12.5v15l12-7.5-12-7.5Z" fill="#fff" />
    </svg>
  );
}
