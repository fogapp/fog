// An attachment is a sealed blob until this device opens it: the key is in
// the message, the bytes are on the hub. Its bytes and its stated type are
// the sender's to choose, so nothing here lets them run in fog's origin:
// an image is shown only when its decrypted bytes prove it is an inert
// type (inlineImageType — never SVG, whatever the label says), and only as
// an <img>, which runs nothing. Everything else is saved, never opened: a
// blob typed application/octet-stream, behind the download attribute. No
// blob URL is ever navigated to.

import { useEffect, useState } from "react";
import { DOWNLOAD_TYPE, inlineImageType, type Attachment, type ChatClient } from "@fog/chat-sdk";
import { formatBytes } from "../../lib/format";

export default function AttachmentView({ client, att }: { client: ChatClient; att: Attachment }) {
  const [url, setUrl] = useState("");
  const [error, setError] = useState("");
  const [notImage, setNotImage] = useState(false);
  // The stated type only decides whether to try: the bytes decide the rest.
  const tryInline = (att.mime ?? "").startsWith("image/");

  useEffect(() => {
    let alive = true;
    let obj = "";
    if (!tryInline) return;
    client
      .openAttachment(att)
      .then((bytes) => {
        if (!alive) return;
        const type = inlineImageType(bytes);
        if (!type) {
          setNotImage(true);
          return;
        }
        obj = URL.createObjectURL(new Blob([bytes as BlobPart], { type }));
        setUrl(obj);
      })
      .catch((e) => alive && setError(e instanceof Error ? e.message : "Couldn't open"));
    return () => {
      alive = false;
      if (obj) URL.revokeObjectURL(obj);
    };
  }, [client, att, tryInline]);

  async function download() {
    try {
      const bytes = await client.openAttachment(att);
      const obj = URL.createObjectURL(new Blob([bytes as BlobPart], { type: DOWNLOAD_TYPE }));
      const a = document.createElement("a");
      a.href = obj;
      a.download = (att.name ?? "attachment").replace(/[/\\]/g, "_");
      a.rel = "noopener";
      a.click();
      setTimeout(() => URL.revokeObjectURL(obj), 10_000);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Couldn't open");
    }
  }

  if (url) {
    return (
      <button type="button" onClick={download} title={att.name ? `Save ${att.name}` : "Save"} className="block">
        <img src={url} alt={att.name ?? ""} className="max-h-72 max-w-xs rounded-md border border-hair" />
      </button>
    );
  }
  return (
    <button onClick={download} className="flex items-center gap-2 rounded-md border border-hair px-2 py-1 text-xs text-ink hover:border-edge">
      <span>📎</span>
      <span className="max-w-[16rem] truncate">{att.name ?? "attachment"}</span>
      {att.size ? <span className="text-ink-muted">{formatBytes(att.size)}</span> : null}
      {tryInline && !notImage && !error && <span className="text-ink-muted">decrypting…</span>}
      {error && <span className="text-warn">{error}</span>}
    </button>
  );
}
