// One glyph per file category, drawn inline so the Drive page carries no
// icon library. The folder is filled; everything else is an outlined page
// with a small mark that says what is inside it.

import type { FileCategory } from "../lib/fileKinds";

const TONES: Record<FileCategory, string> = {
  folder: "text-accent",
  apps: "text-accent",
  content: "text-accent",
  image: "text-accent",
  video: "text-ink",
  audio: "text-warn",
  pdf: "text-warn",
  text: "text-ink-muted",
  code: "text-warn",
  archive: "text-ink-muted",
  document: "text-accent",
  link: "text-ink-muted",
  file: "text-ink-muted",
};

export default function FileIcon({ category, className = "h-5 w-5" }: { category: FileCategory; className?: string }) {
  const cls = `${className} shrink-0 ${TONES[category]}`;
  if (category === "folder") {
    return (
      <svg viewBox="0 0 24 24" className={cls} fill="currentColor" aria-hidden>
        <path d="M3 6.5A2.5 2.5 0 0 1 5.5 4h3.6a2.5 2.5 0 0 1 1.77.73L12.6 6.5h5.9A2.5 2.5 0 0 1 21 9v8.5a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5v-11Z" />
      </svg>
    );
  }
  if (category === "apps") {
    // The folder, with a four-square app grid cut out of it.
    return (
      <svg viewBox="0 0 24 24" className={cls} fill="currentColor" aria-hidden>
        <path fillRule="evenodd" d="M3 6.5A2.5 2.5 0 0 1 5.5 4h3.6a2.5 2.5 0 0 1 1.77.73L12.6 6.5h5.9A2.5 2.5 0 0 1 21 9v8.5a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5v-11Zm5.5 3.5a1 1 0 0 0-1 1v1.5a1 1 0 0 0 1 1H10a1 1 0 0 0 1-1V11a1 1 0 0 0-1-1H8.5Zm5.5 0a1 1 0 0 0-1 1v1.5a1 1 0 0 0 1 1h1.5a1 1 0 0 0 1-1V11a1 1 0 0 0-1-1H14Zm-5.5 4.5a1 1 0 0 0-1 1V17a1 1 0 0 0 1 1H10a1 1 0 0 0 1-1v-1.5a1 1 0 0 0-1-1H8.5Zm5.5 0a1 1 0 0 0-1 1V17a1 1 0 0 0 1 1h1.5a1 1 0 0 0 1-1v-1.5a1 1 0 0 0-1-1H14Z" />
      </svg>
    );
  }
  if (category === "content") {
    // The folder, with a play mark cut out of it: the place films, music
    // and photos live, whichever app happens to read them.
    return (
      <svg viewBox="0 0 24 24" className={cls} fill="currentColor" aria-hidden>
        <path fillRule="evenodd" d="M3 6.5A2.5 2.5 0 0 1 5.5 4h3.6a2.5 2.5 0 0 1 1.77.73L12.6 6.5h5.9A2.5 2.5 0 0 1 21 9v8.5a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5v-11Zm7 4.19a.6.6 0 0 1 .9-.52l4.42 2.56a.6.6 0 0 1 0 1.04l-4.42 2.56a.6.6 0 0 1-.9-.52v-5.12Z" />
      </svg>
    );
  }
  if (category === "link") {
    return (
      <svg viewBox="0 0 24 24" className={cls} fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" aria-hidden>
        <path d="M10 14 14 10M8.5 15.5l-1.4 1.4a3 3 0 0 1-4.2-4.2l3-3a3 3 0 0 1 4.2 0M15.5 8.5l1.4-1.4a3 3 0 0 1 4.2 4.2l-3 3a3 3 0 0 1-4.2 0" />
      </svg>
    );
  }
  return (
    <svg viewBox="0 0 24 24" className={cls} fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8l-5-5Z" />
      <path d="M14 3v5h5" />
      {mark(category)}
    </svg>
  );
}

function mark(category: FileCategory) {
  switch (category) {
    case "image":
      return <path d="M8 17l2.5-3 2 2.2L15 12l3 5H8Z" fill="currentColor" stroke="none" />;
    case "video":
      return <path d="M10 11.5v5l4-2.5-4-2.5Z" fill="currentColor" stroke="none" />;
    case "audio":
      return <path d="M10 17a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Zm1.5-1.5V11l4-1v4.5" />;
    case "pdf":
      return <path d="M8.5 17v-5h1.6a1.3 1.3 0 0 1 0 2.6H8.5M12.7 12v5M15 17v-5h2.5M15 14.5h2" strokeWidth="1.4" />;
    case "text":
      return <path d="M8.5 12.5h7M8.5 15h7M8.5 17.5h4.5" strokeWidth="1.4" />;
    case "code":
      return <path d="m10 12-2.5 2.5L10 17m4-5 2.5 2.5L14 17" strokeWidth="1.6" />;
    case "archive":
      return <path d="M11 9v1.5M11 12v1.5M11 15v1.5M11 18v1" strokeWidth="1.8" />;
    case "document":
      return <path d="M8.5 12.5h7M8.5 15h7M8.5 17.5h7" strokeWidth="1.4" />;
    default:
      return null;
  }
}
