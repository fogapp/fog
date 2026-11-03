import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

// Markdown for app notes (catalog entries, fog.yaml) and the markdown a
// repo ships in its root. GitHub's dialect — tables, task lists, strike,
// bare links — because that is what a README is written in. react-markdown
// never injects raw HTML, so anything script-shaped in a note renders as
// text. Element styling lives here so every note reads the same. The
// parser is a chunk of its own; pages import the Markdown wrapper.
export default function MarkdownImpl({ children }: { children: string }) {
  return (
    <div className="space-y-2 text-sm leading-relaxed text-ink">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ children, href }) => (
            <a href={href} target="_blank" rel="noreferrer" className="text-accent underline hover:text-accent">
              {children}
            </a>
          ),
          h1: ({ children }) => <p className="text-base font-semibold text-ink">{children}</p>,
          h2: ({ children }) => <p className="text-base font-semibold text-ink">{children}</p>,
          h3: ({ children }) => <p className="font-semibold text-ink">{children}</p>,
          h4: ({ children }) => <p className="font-semibold text-ink">{children}</p>,
          strong: ({ children }) => <strong className="font-semibold text-ink">{children}</strong>,
          ul: ({ children }) => <ul className="list-disc space-y-1 pl-5">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal space-y-1 pl-5">{children}</ol>,
          code: ({ children }) => (
            <code className="rounded border border-hair bg-code px-1 py-0.5 font-mono text-xs text-ink">{children}</code>
          ),
          pre: ({ children }) => (
            <pre className="overflow-x-auto rounded-lg border border-hair bg-code p-3 text-xs [&_code]:border-0 [&_code]:bg-transparent [&_code]:p-0">
              {children}
            </pre>
          ),
          blockquote: ({ children }) => (
            <blockquote className="border-l-2 border-edge pl-3 text-ink-muted">{children}</blockquote>
          ),
          hr: () => <hr className="border-hair" />,
          table: ({ children }) => (
            <div className="overflow-x-auto">
              <table className="w-full text-left text-cell">{children}</table>
            </div>
          ),
          thead: ({ children }) => <thead className="caps-label border-b border-hair-strong">{children}</thead>,
          th: ({ children }) => <th className="py-2 pr-3 font-medium">{children}</th>,
          td: ({ children }) => <td className="border-b border-divider py-2 pr-3 align-top">{children}</td>,
          del: ({ children }) => <del className="text-ink-muted">{children}</del>,
          input: ({ checked }) => (
            <input type="checkbox" checked={!!checked} readOnly className="mr-1.5 align-middle accent-accent" />
          ),
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
