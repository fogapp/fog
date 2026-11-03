// The Finder selection model over an ordered entry list: a focus (the row
// the keyboard is on), an anchor (where a shift-range starts), and the set
// of selected paths. Click selects, ⌘-click toggles, ⇧-click ranges; arrows
// move the focus and, with shift, extend from the anchor. Kept by path so a
// refetch that reorders rows keeps the right things selected.

import { useCallback, useMemo, useRef, useState } from "react";
import type { FileNode } from "../../api";

export interface Selection {
  selected: Set<string>;
  focus: number;
  isSelected: (path: string) => boolean;
  click: (i: number, e: { shiftKey: boolean; metaKey: boolean; ctrlKey: boolean }) => void;
  move: (delta: number, extend: boolean) => number;
  jump: (i: number, extend: boolean) => number;
  selectAll: () => void;
  clear: () => void;
  set: (paths: string[], focus?: number) => void;
  // Type-ahead: letters typed in quick succession jump to the first name
  // starting with them, the way every desktop file manager does.
  typeAhead: (ch: string) => number;
}

export function useSelection(entries: FileNode[]): Selection {
  const [rawSelected, setSelected] = useState<Set<string>>(() => new Set());
  const [rawFocus, setFocus] = useState(-1);
  const anchor = useRef(-1);
  const typed = useRef({ buf: "", at: 0 });

  // Entries that vanished (deleted, moved, folder changed) leave the
  // selection and the focus clamps to what is left — derived during
  // render, not pruned in an effect, so no keystroke can ever observe a
  // selection of things that are no longer on screen.
  const paths = useMemo(() => entries.map((e) => e.path), [entries]);
  const selected = useMemo(() => {
    if (rawSelected.size === 0) return rawSelected;
    const keep = new Set<string>();
    for (const p of paths) if (rawSelected.has(p)) keep.add(p);
    return keep.size === rawSelected.size ? rawSelected : keep;
  }, [rawSelected, paths]);
  const focus = rawFocus >= paths.length ? paths.length - 1 : rawFocus;

  const isSelected = useCallback((p: string) => selected.has(p), [selected]);

  const range = useCallback(
    (a: number, b: number) => {
      const lo = Math.max(0, Math.min(a, b));
      const hi = Math.min(paths.length - 1, Math.max(a, b));
      const next = new Set<string>();
      for (let i = lo; i <= hi; i++) next.add(paths[i]);
      return next;
    },
    [paths],
  );

  const click: Selection["click"] = useCallback(
    (i, e) => {
      if (e.shiftKey && anchor.current >= 0) {
        setSelected(range(anchor.current, i));
      } else if (e.metaKey || e.ctrlKey) {
        setSelected((prev) => {
          const next = new Set(prev);
          if (next.has(paths[i])) next.delete(paths[i]);
          else next.add(paths[i]);
          return next;
        });
        anchor.current = i;
      } else {
        setSelected(new Set([paths[i]]));
        anchor.current = i;
      }
      setFocus(i);
    },
    [paths, range],
  );

  const jump: Selection["jump"] = useCallback(
    (i, extend) => {
      if (paths.length === 0) return -1;
      const to = Math.max(0, Math.min(paths.length - 1, i));
      if (extend && anchor.current >= 0) {
        setSelected(range(anchor.current, to));
      } else {
        setSelected(new Set([paths[to]]));
        anchor.current = to;
      }
      setFocus(to);
      return to;
    },
    [paths, range],
  );

  const move: Selection["move"] = useCallback(
    (delta, extend) => jump(focus < 0 ? (delta > 0 ? 0 : paths.length - 1) : focus + delta, extend),
    [focus, jump, paths.length],
  );

  const selectAll = useCallback(() => setSelected(new Set(paths)), [paths]);
  const clear = useCallback(() => {
    setSelected(new Set());
    anchor.current = -1;
  }, []);
  const set: Selection["set"] = useCallback((ps, f) => {
    setSelected(new Set(ps));
    if (f !== undefined) {
      setFocus(f);
      anchor.current = f;
    }
  }, []);

  const typeAhead: Selection["typeAhead"] = useCallback(
    (ch) => {
      const now = Date.now();
      const t = typed.current;
      t.buf = now - t.at < 900 ? t.buf + ch : ch;
      t.at = now;
      const needle = t.buf.toLowerCase();
      const from = t.buf.length === 1 ? focus + 1 : 0;
      const order = [...entries.slice(from), ...entries.slice(0, from)];
      const hit = order.findIndex((e) => e.name.toLowerCase().startsWith(needle));
      if (hit < 0) return -1;
      return jump((from + hit) % entries.length, false);
    },
    [entries, focus, jump],
  );

  return { selected, focus, isSelected, click, move, jump, selectAll, clear, set, typeAhead };
}
