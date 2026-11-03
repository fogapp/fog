import { Suspense, lazy } from "react";

// The markdown parser and its GitHub dialect are a chunk of their own
// (MarkdownImpl): most pages that show a note are not first paint, and
// the login screen should not carry a parser. Until it arrives the note
// reads as the plain text it is.
const MarkdownImpl = lazy(() => import("./MarkdownImpl"));

export default function Markdown({ children }: { children: string }) {
  return (
    <Suspense fallback={<div className="whitespace-pre-wrap text-sm leading-relaxed text-ink">{children}</div>}>
      <MarkdownImpl>{children}</MarkdownImpl>
    </Suspense>
  );
}
