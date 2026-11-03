import { Suspense, lazy } from "react";

// A message's text, as markdown. Its own renderer, not the app notes' one:
// a message is a stranger's, so no image it names is loaded (a reader's
// browser fetching a sender's URL tells them who read and when), and a
// link is a link only if it is http(s) or mailto (safeHref) — never
// javascript:, data: or a path into fog. The parser is a chunk of its own.
const ChatMarkdownImpl = lazy(() => import("./ChatMarkdownImpl"));

export default function ChatMarkdown({ children }: { children: string }) {
  return (
    <Suspense fallback={<div className="whitespace-pre-wrap text-sm leading-relaxed text-ink">{children}</div>}>
      <ChatMarkdownImpl>{children}</ChatMarkdownImpl>
    </Suspense>
  );
}
