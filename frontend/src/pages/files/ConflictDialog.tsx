// A name is taken at the destination. The server said which entry is in
// the way; the operator says what to do with the whole batch — one answer,
// not one dialog per file.

import type { FilesConflict, FilesConflictPolicy } from "../../api";
import Button from "../../components/Button";
import Modal from "../../components/Modal";
import { formatBytes, formatWhen } from "../../lib/format";
import { displayName, FileName } from "../../lib/displayName";

export default function ConflictDialog({
  verb,
  count,
  conflict,
  onChoose,
  onClose,
}: {
  verb: "move" | "copy" | "restore";
  count: number;
  conflict: FilesConflict;
  onChoose: (policy: Exclude<FilesConflictPolicy, "">) => void;
  onClose: () => void;
}) {
  const e = conflict.existing;
  return (
    <Modal title={`“${displayName(e.name)}” already exists`} onClose={onClose}>
      <div className="space-y-4 text-sm">
        <p className="text-ink">
          There is already a {e.kind === "dir" ? "folder" : "file"} named <span className="font-medium text-ink"><FileName name={e.name} /></span> in the
          destination{e.kind === "file" ? ` (${formatBytes(e.size)}, modified ${formatWhen(e.modified)})` : ""}.
          {count > 1 && " The same choice will apply to every conflict in this batch."}
        </p>
        <div className="flex flex-wrap gap-2">
          <Button variant="primary" onClick={() => onChoose("rename")}>
            Keep both
          </Button>
          <Button variant="danger" onClick={() => onChoose("replace")}>
            Replace
          </Button>
          <Button onClick={() => onChoose("skip")}>Skip</Button>
          <Button variant="ghost" onClick={onClose}>
            Cancel {verb}
          </Button>
        </div>
        <p className="text-xs text-ink-muted">Replace sends the existing entry to the Trash, so it can still be brought back.</p>
      </div>
    </Modal>
  );
}
