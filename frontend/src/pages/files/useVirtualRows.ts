// Windowing for the entry list: only the rows in (and just around) the
// viewport are mounted, so a folder of ten thousand entries scrolls like a
// folder of ten. Rows are a fixed height, which is what makes the arithmetic
// trivial and the scrollbar honest; no library needed for that. The hook
// measures the box; the window itself is a pure function of those numbers,
// because in grid view the row count depends on the measured width.

import { useCallback, useEffect, useRef, useState } from "react";

export function useScrollBox() {
  const ref = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [height, setHeight] = useState(600);
  const [width, setWidth] = useState(800);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const onScroll = () => setScrollTop(el.scrollTop);
    el.addEventListener("scroll", onScroll, { passive: true });
    const ro = new ResizeObserver(() => {
      setHeight(el.clientHeight);
      setWidth(el.clientWidth);
    });
    ro.observe(el);
    setHeight(el.clientHeight);
    setWidth(el.clientWidth);
    return () => {
      el.removeEventListener("scroll", onScroll);
      ro.disconnect();
    };
  }, []);

  // Keep a row in view when the keyboard moves the focus to it.
  const scrollToRow = useCallback((row: number, rowHeight: number, headerHeight = 0) => {
    const el = ref.current;
    if (!el) return;
    const top = row * rowHeight + headerHeight;
    const bottom = top + rowHeight;
    if (top - headerHeight < el.scrollTop) el.scrollTop = top - headerHeight;
    else if (bottom > el.scrollTop + el.clientHeight) el.scrollTop = bottom - el.clientHeight;
  }, []);

  return { ref, scrollTop, height, width, scrollToRow };
}

export interface Window {
  start: number;
  end: number;
  topPad: number;
  bottomPad: number;
}

export function windowRows(scrollTop: number, height: number, count: number, rowHeight: number, overscan = 12): Window {
  const start = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const end = Math.min(count, Math.ceil((scrollTop + height) / rowHeight) + overscan);
  return { start, end, topPad: start * rowHeight, bottomPad: Math.max(0, (count - end) * rowHeight) };
}
