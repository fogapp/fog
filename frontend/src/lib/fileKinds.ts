// What kind of thing a file is, for the icon, the preview, and the "Kind"
// column — derived from the type the server guessed by extension, so the
// page and the API can never disagree about it.

import type { FileNode } from "../api";

const FILE_CATEGORIES = [
  "folder", "apps", "content", "image", "video", "audio", "pdf", "text", "code", "archive", "document", "link", "file",
] as const;
export type FileCategory = (typeof FILE_CATEGORIES)[number];

const CATEGORY_LABELS: Record<FileCategory, string> = {
  folder: "Folder",
  apps: "App folders",
  content: "Content",
  image: "Image",
  video: "Video",
  audio: "Audio",
  pdf: "PDF",
  text: "Text",
  code: "Code",
  archive: "Archive",
  document: "Document",
  link: "Link",
  file: "File",
};

const CODE = new Set([
  "text/x-go", "text/x-ruby", "text/x-python", "text/x-shellscript", "text/x-rust", "text/x-sql", "text/x-c",
  "text/x-c++", "text/x-java", "text/x-swift", "text/typescript", "text/javascript", "application/json",
  "application/xml", "text/css", "text/html", "text/yaml",
]);

const DOCUMENT_PREFIXES = [
  "application/msword", "application/vnd.openxmlformats", "application/vnd.ms-", "application/vnd.oasis",
  "application/x-iwork", "application/epub",
];

const ARCHIVE = new Set([
  "application/zip", "application/x-tar", "application/gzip", "application/x-bzip2", "application/x-xz",
  "application/zstd", "application/x-7z-compressed", "application/vnd.rar", "application/x-apple-diskimage",
  "application/x-iso9660-image",
]);

export function categoryOf(n: Pick<FileNode, "kind" | "mime" | "role">): FileCategory {
  // The two folders fog gives a meaning to get their own glyphs, so each
  // reads as what it is rather than as one more folder: Apps is where an
  // app's own data lives, Content is where your libraries do.
  if (n.kind === "dir") {
    if (n.role === "apps") return "apps";
    if (n.role === "content") return "content";
    return "folder";
  }
  if (n.kind === "other") return "link";
  const m = n.mime ?? "";
  if (m.startsWith("image/")) return "image";
  if (m.startsWith("video/")) return "video";
  if (m.startsWith("audio/")) return "audio";
  if (m === "application/pdf") return "pdf";
  if (CODE.has(m)) return "code";
  if (m.startsWith("text/")) return "text";
  if (ARCHIVE.has(m)) return "archive";
  if (DOCUMENT_PREFIXES.some((p) => m.startsWith(p))) return "document";
  return "file";
}

// The "Kind" column: the category, refined by extension where that is
// what people actually call the thing ("JPEG image", "MP4 video").
export function kindLabel(n: Pick<FileNode, "kind" | "mime" | "name" | "role">): string {
  const cat = categoryOf(n);
  if (cat === "folder" && n.role === "app-folder") return "App folder";
  if (cat === "folder" && n.role === "library") return "Content library";
  if (cat === "folder" || cat === "apps" || cat === "content" || cat === "link") return CATEGORY_LABELS[cat];
  const ext = n.name.includes(".") ? n.name.slice(n.name.lastIndexOf(".") + 1).toUpperCase() : "";
  if (!ext || ext.length > 5) return CATEGORY_LABELS[cat];
  if (cat === "file") return `${ext} file`;
  if (cat === "pdf") return "PDF";
  return `${ext} ${CATEGORY_LABELS[cat].toLowerCase()}`;
}

// What Quick Look can render inline. Text is capped: a log file of a
// gigabyte is a download, not a preview.
const TEXT_PREVIEW_MAX = 1024 * 1024;

export function previewable(n: FileNode): boolean {
  const cat = categoryOf(n);
  switch (cat) {
    case "image":
    case "video":
    case "audio":
    case "pdf":
      return true;
    case "text":
    case "code":
      return n.size <= TEXT_PREVIEW_MAX;
    default:
      return false;
  }
}
