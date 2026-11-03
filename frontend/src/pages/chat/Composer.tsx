// The composer. Enter sends, Shift+Enter breaks a line; @ opens a picker of
// the room's people and the chosen ones travel as mention hints beside the
// ciphertext (the one deliberate leak, so a locked phone can buzz). Files
// are encrypted here, uploaded, then referenced from the message. Callers
// key it by room (and thread), so a draft never follows you to another.

import { useEffect, useRef, useState } from "react";
import type { Attachment, ChatClient, DirectoryPerson, ParticipantView, PreviewCard } from "@fog/chat-sdk";
import { errorText } from "../../components/Async";
import { handleOf, nameFor } from "./names";
import PreviewCardView from "./PreviewCardView";

const URL_RE = /https?:\/\/[^\s<>"')\]]+/i;

export default function Composer({
  client, roomId, participants, directory, topicId, placeholder,
}: {
  client: ChatClient;
  roomId: string;
  participants: ParticipantView[];
  directory: DirectoryPerson[];
  topicId?: string;
  placeholder?: string;
}) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [pending, setPending] = useState<Attachment[]>([]);
  const [uploading, setUploading] = useState(0);
  const [mentionQ, setMentionQ] = useState<string | null>(null);
  // The link card for the first URL in the draft, fetched with fog's help
  // once the person has stopped typing — only if they turned previews on.
  const [preview, setPreview] = useState<PreviewCard | null>(null);
  const previewFor = useRef("");
  const dismissed = useRef("");
  const ta = useRef<HTMLTextAreaElement>(null);
  const file = useRef<HTMLInputElement>(null);
  const lastTyping = useRef(0);
  const me = client.me().addr;

  useEffect(() => {
    if (!client.cachedPrefs()?.previews) return;
    const url = URL_RE.exec(text)?.[0] ?? "";
    if (!url || url === dismissed.current) {
      if (!url) {
        setPreview(null);
        previewFor.current = "";
      }
      return;
    }
    if (url === previewFor.current) return;
    const t = setTimeout(() => {
      previewFor.current = url;
      client
        .preview(url)
        .then((card) => {
          if (previewFor.current === url && card && card.url) setPreview({ url: card.url, title: card.title, description: card.description, image: card.image, site: card.site });
        })
        .catch(() => {
          /* no card: the page had none, or it was not a public address */
        });
    }, 600);
    return () => clearTimeout(t);
  }, [client, text]);

  const others = participants.filter((p) => !p.leftSeq && p.addr !== me);
  const suggestions = mentionQ === null ? [] : others.filter((p) => p.handle.startsWith(mentionQ) || p.displayName.toLowerCase().startsWith(mentionQ)).slice(0, 6);

  function onChange(v: string) {
    setText(v);
    const m = /(?:^|\s)@([a-z0-9_.-]*)$/i.exec(v);
    setMentionQ(m ? m[1].toLowerCase() : null);
    const now = Date.now();
    if (now - lastTyping.current > 3000 && v.trim()) {
      lastTyping.current = now;
      client.typing(roomId);
    }
  }

  function pick(p: ParticipantView) {
    setText((t) => t.replace(/@([a-z0-9_.-]*)$/i, `@${p.handle} `));
    setMentionQ(null);
    ta.current?.focus();
  }

  async function send() {
    const body = text.trim();
    if ((!body && pending.length === 0) || busy) return;
    setBusy(true);
    setError("");
    // Mentions: every @handle in the text that names a participant.
    const mentions = others.filter((p) => new RegExp(`(^|\\s)@${p.handle.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(\\b|$)`, "i").test(body)).map((p) => p.addr);
    try {
      await client.send(roomId, body, { topicId, mentions, attachments: pending, preview: preview ?? undefined });
      setText("");
      setPending([]);
      setPreview(null);
      previewFor.current = "";
      dismissed.current = "";
    } catch (e) {
      setError(errorText(e, "Couldn't send"));
    } finally {
      setBusy(false);
      ta.current?.focus();
    }
  }

  async function attach(files: FileList | null) {
    if (!files) return;
    setError("");
    for (const f of Array.from(files)) {
      setUploading((n) => n + 1);
      try {
        const bytes = new Uint8Array(await f.arrayBuffer());
        const att = await client.uploadAttachment(roomId, bytes, f.name, f.type || "application/octet-stream");
        setPending((p) => [...p, att]);
      } catch (e) {
        setError(errorText(e, `Couldn't attach ${f.name}`));
      } finally {
        setUploading((n) => n - 1);
      }
    }
  }

  return (
    <div className="border-t border-hair px-4 py-3 sm:px-6">
      {pending.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-2">
          {pending.map((a) => (
            <span key={a.blob} className="flex items-center gap-1 rounded-md border border-hair px-2 py-0.5 text-xs text-ink">
              📎 {a.name}
              <button onClick={() => setPending((p) => p.filter((x) => x.blob !== a.blob))} className="text-ink-muted hover:text-ink" aria-label={`Remove ${a.name}`}>
                ✕
              </button>
            </span>
          ))}
        </div>
      )}
      {preview && (
        <PreviewCardView
          card={preview}
          onRemove={() => {
            dismissed.current = preview.url;
            setPreview(null);
          }}
        />
      )}
      <div className="relative">
        {suggestions.length > 0 && (
          <ul className="absolute bottom-full left-0 mb-1 w-64 rounded-md border border-veil-edge bg-fog-900 py-1 shadow-xl">
            {suggestions.map((p) => (
              <li key={p.addr}>
                <button onMouseDown={(e) => { e.preventDefault(); pick(p); }} className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-cell text-ink hover:bg-hover">
                  <span>{nameFor(p.addr, directory, client)}</span>
                  <span className="font-mono text-xs text-ink-muted">@{handleOf(p.addr)}</span>
                </button>
              </li>
            ))}
          </ul>
        )}
        <div className="flex items-end gap-2 rounded-lg border border-edge bg-veil px-2 py-1.5 focus-within:border-accent">
          <button
            onClick={() => file.current?.click()}
            title="Attach a file (encrypted before upload)"
            aria-label="Attach a file"
            className="rounded px-1.5 py-1 text-ink-muted hover:bg-hover hover:text-ink">
            📎
          </button>
          <input ref={file} type="file" multiple hidden onChange={(e) => attach(e.target.files)} />
          <textarea
            ref={ta}
            value={text}
            onChange={(e) => onChange(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Tab" && suggestions.length) {
                e.preventDefault();
                pick(suggestions[0]);
              } else if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                if (suggestions.length && mentionQ) pick(suggestions[0]);
                else send();
              } else if (e.key === "Escape") setMentionQ(null);
            }}
            rows={Math.min(8, Math.max(1, text.split("\n").length))}
            placeholder={placeholder ?? "Message — Enter to send, Shift+Enter for a new line"}
            className="min-h-[1.75rem] flex-1 resize-none bg-transparent py-1 text-sm text-ink placeholder:text-ink-muted focus:outline-none"
          />
          <button
            onClick={send}
            disabled={busy || uploading > 0 || (!text.trim() && pending.length === 0)}
            className="rounded-md bg-accent px-3 py-1 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-40">
            {uploading > 0 ? "Encrypting…" : busy ? "Sending…" : "Send"}
          </button>
        </div>
      </div>
      {error && <p className="mt-1 text-hint text-warn">{error}</p>}
    </div>
  );
}
