// Space bar on a file: a preview the size of the screen, with ← → to walk
// the folder without leaving it. Images, video and audio come straight
// from /raw (Range requests make scrubbing work); PDFs through the
// browser's viewer; text and code inline up to a size cap; everything else
// shows what it is and offers the download.

import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import type { FileNode } from "../../api";
import Markdown from "../../components/Markdown";
import { Loading } from "../../components/Async";
import { categoryOf, kindLabel, previewable } from "../../lib/fileKinds";
import { formatBytes, formatWhen } from "../../lib/format";
import { filesKey, rawUrl } from "../../lib/useFiles";
import FileIcon from "../../components/FileIcon";
import { displayName, FileName } from "../../lib/displayName";

export default function QuickLook({
  entries,
  index,
  at,
  onMove,
  onClose,
}: {
  entries: FileNode[];
  index: number;
  at?: number;
  onMove: (i: number) => void;
  onClose: () => void;
}) {
  const n = entries[index];
  // The keydown that opened the preview is still bubbling when this
  // listener is attached (React flushes the render inside the event), and
  // it must not be the keydown that closes it.
  const openedAt = useRef(performance.now());

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.timeStamp <= openedAt.current) return;
      if (e.key === "Escape" || e.key === " ") {
        e.preventDefault();
        onClose();
      } else if (e.key === "ArrowRight" || e.key === "ArrowDown") {
        e.preventDefault();
        if (index < entries.length - 1) onMove(index + 1);
      } else if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
        e.preventDefault();
        if (index > 0) onMove(index - 1);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [index, entries.length, onMove, onClose]);

  // The pane may already be playing this file inline; one player at a
  // time, so the preview does not sound twice.
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    document.querySelectorAll<HTMLMediaElement>("video, audio").forEach((m) => {
      if (!root.current?.contains(m)) m.pause();
    });
  }, [n?.path]);

  if (!n) return null;

  return (
    // The backdrop is black in both themes, so its chrome uses fixed
    // colours rather than the positional tokens (which flip in light mode).
    <div ref={root} className="fixed inset-0 z-50 flex flex-col bg-black/85 backdrop-blur-sm" onClick={onClose}>
      <div className="flex h-12 shrink-0 items-center gap-3 px-4 text-sm text-white" onClick={(e) => e.stopPropagation()}>
        <FileIcon category={categoryOf(n)} className="h-5 w-5" />
        <span className="min-w-0 truncate font-medium"><FileName name={n.name} /></span>
        <span className="text-white/60">{n.kind === "dir" ? `${n.entries ?? 0} items` : formatBytes(n.size)} · {formatWhen(n.modified)}</span>
        <span className="ml-auto text-xs text-white/50">
          {index + 1} of {entries.length} · ← → · esc
        </span>
        {n.kind === "file" && (
          <a href={rawUrl(n.path, true, at)} className="rounded-md border border-white/30 px-2.5 py-1 text-xs font-medium text-white hover:bg-white/10">
            Download
          </a>
        )}
        <button onClick={onClose} className="px-1 text-white/60 hover:text-white" title="Close">✕</button>
      </div>
      <div className="flex min-h-0 flex-1 items-center justify-center p-4" onClick={(e) => e.stopPropagation()}>
        <Preview n={n} at={at} />
      </div>
    </div>
  );
}

function Preview({ n, at }: { n: FileNode; at?: number }) {
  const cat = categoryOf(n);
  if (n.kind !== "file" || !previewable(n)) return <NoPreview n={n} at={at} />;
  const src = rawUrl(n.path, false, at);
  switch (cat) {
    case "image":
      return <img src={src} alt={displayName(n.name)} className="max-h-full max-w-full rounded object-contain shadow-2xl" />;
    case "video":
      // A player a little smaller than the window on the dark backdrop:
      // the box is fixed, the picture letterboxes inside it, so a 9:16
      // phone clip and a widescreen film get the same frame and the same
      // full-width controls. Keyed by path so ← → loads the next file
      // fresh rather than swapping the source under a running player.
      return (
        <div className="flex h-full w-full items-center justify-center px-2 py-1 sm:px-10 sm:py-5">
          <video
            key={`${at ?? 0}:${n.path}`}
            src={src}
            controls
            autoPlay
            playsInline
            className="h-full w-full rounded-lg bg-black object-contain shadow-2xl ring-1 ring-white/10"
          />
        </div>
      );
    case "audio":
      return (
        <div className="flex flex-col items-center gap-6">
          <FileIcon category="audio" className="h-24 w-24" />
          <audio src={src} controls autoPlay className="w-[28rem] max-w-full" />
        </div>
      );
    case "pdf":
      return <iframe src={src} title={displayName(n.name)} className="h-full w-full max-w-5xl rounded bg-white" />;
    case "text":
    case "code":
      return <TextPreview n={n} at={at} />;
    default:
      return <NoPreview n={n} at={at} />;
  }
}

function TextPreview({ n, at }: { n: FileNode; at?: number }) {
  const text = useQuery({
    queryKey: [...filesKey, "text", at ?? 0, n.path, n.modified, n.size],
    queryFn: async () => {
      const res = await fetch(rawUrl(n.path, false, at), { credentials: "same-origin" });
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      return res.text();
    },
    staleTime: 60_000,
  });
  if (text.isPending) return <Loading label="Reading…" className="text-white/70" />;
  if (text.isError) return <p className="text-sm text-warn">Could not read the file.</p>;
  const md = n.mime === "text/markdown";
  return (
    <div className="h-full w-full max-w-4xl overflow-auto rounded-lg border border-hair bg-fog-950 p-5">
      {md ? (
        <Markdown>{text.data}</Markdown>
      ) : (
        <pre className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed text-ink">{text.data}</pre>
      )}
    </div>
  );
}

function NoPreview({ n, at }: { n: FileNode; at?: number }) {
  return (
    <div className="flex flex-col items-center gap-4 text-center">
      <FileIcon category={categoryOf(n)} className="h-24 w-24" />
      <p className="text-lg font-medium text-white"><FileName name={n.name} /></p>
      <p className="text-sm text-white/60">
        {kindLabel(n)} · {formatBytes(n.size)}
        {n.kind === "file" && n.size > 1024 * 1024 && (n.mime?.startsWith("text/") ?? false) ? " · too large to preview" : ""}
      </p>
      {n.kind === "file" && (
        <a href={rawUrl(n.path, true, at)} className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover">
          Download
        </a>
      )}
    </div>
  );
}
