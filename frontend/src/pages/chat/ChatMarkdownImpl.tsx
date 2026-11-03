import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { safeHref } from "@fog/chat-sdk";

// See ChatMarkdown. react-markdown never injects raw HTML; on top of that
// every URL goes through safeHref, and images become a plain link to what
// they name, fetched only if the reader follows it.
export default function ChatMarkdownImpl({ children }: { children: string }) {
  return (
    <div className="space-y-2 text-sm leading-relaxed text-ink">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        urlTransform={(url) => safeHref(url) ?? ""}
        components={{
          a: ({ children, href }) => {
            const safe = safeHref(href);
            return safe ? (
              <a href={safe} target="_blank" rel="noopener noreferrer" className="text-accent underline hover:text-accent">
                {children}
              </a>
            ) : (
              <span>{children}</span>
            );
          },
          img: ({ src, alt }) => {
            const safe = safeHref(typeof src === "string" ? src : undefined);
            return safe ? (
              <a href={safe} target="_blank" rel="noopener noreferrer" className="text-accent underline">
                {alt || "image"}
              </a>
            ) : (
              <span>{alt}</span>
            );
          },
          strong: ({ children }) => <strong className="font-semibold text-ink">{children}</strong>,
          ul: ({ children }) => <ul className="list-disc space-y-1 pl-5">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal space-y-1 pl-5">{children}</ol>,
          code: ({ children }) => <code className="rounded border border-hair bg-code px-1 py-0.5 font-mono text-xs text-ink">{children}</code>,
          pre: ({ children }) => (
            <pre className="overflow-x-auto rounded-lg border border-hair bg-code p-3 text-xs [&_code]:border-0 [&_code]:bg-transparent [&_code]:p-0">{children}</pre>
          ),
          blockquote: ({ children }) => <blockquote className="border-l-2 border-edge pl-3 text-ink-muted">{children}</blockquote>,
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
