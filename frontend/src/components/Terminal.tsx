import { useEffect, useRef, useState } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { useTheme } from "../theme";
import { control } from "./Field";

// xterm paints its own canvas, so it can't inherit the page's tokens — it
// gets a palette per theme instead. The dark one is the screen-within-a-page
// this view has always used; the light one darkens the ANSI brights, which
// are unreadable on a pale background at their defaults.
const TERM_THEME = {
  dark: { background: "#101218", foreground: "#eceef2", cursor: "#eceef2", selectionBackground: "#3a4153" },
  light: {
    background: "#f7f8fb",
    foreground: "#1f242e",
    cursor: "#1f242e",
    selectionBackground: "#d2d8e3",
    black: "#1f242e", red: "#b3261e", green: "#136f43", yellow: "#8a5a00",
    blue: "#1f5fbf", magenta: "#7c3aed", cyan: "#0e7490", white: "#545e72",
    brightBlack: "#67728a", brightRed: "#c0392b", brightGreen: "#15803d", brightYellow: "#a16207",
    brightBlue: "#1d4ed8", brightMagenta: "#6d28d9", brightCyan: "#0f766e", brightWhite: "#2b3240",
  },
} as const;

export default function Terminal({ appName }: { appName: string }) {
  const { theme } = useTheme();
  const [cmd, setCmd] = useState("/bin/sh");
  const [running, setRunning] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const termRef = useRef<XTerm | null>(null);
  const winResizeRef = useRef<(() => void) | null>(null);

  function stop() {
    // Each start() registers one window-resize listener for its fit addon;
    // it must die here or every Connect/Restart stacks another, and after
    // unmount they'd call fit() on a disposed terminal.
    if (winResizeRef.current) {
      window.removeEventListener("resize", winResizeRef.current);
      winResizeRef.current = null;
    }
    const ws = wsRef.current;
    if (ws) {
      // This close is ours — detach onclose so it can't fire later (after a
      // restart has disposed this terminal) and write into the wreckage. The
      // "[session ended]" line is written here instead, while the terminal
      // is known to be alive.
      ws.onclose = null;
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
        ws.close();
        termRef.current?.write("\r\n\x1b[90m[session ended]\x1b[0m\r\n");
      }
    }
    wsRef.current = null;
    setRunning(false);
  }

  function start(command: string) {
    stop();
    termRef.current?.dispose();
    const term = new XTerm({
      fontSize: 13,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
      theme: TERM_THEME[theme],
      cursorBlink: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(containerRef.current!);
    fit.fit();
    termRef.current = term;

    const proto = window.location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(
      `${proto}://${window.location.host}/api/apps/${appName}/exec?cmd=${encodeURIComponent(command)}`,
    );
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;
    setRunning(true);

    ws.onopen = () => {
      ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
      term.focus();
    };
    ws.onmessage = (ev) => {
      term.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    ws.onclose = () => {
      term.write("\r\n\x1b[90m[session ended]\x1b[0m\r\n");
      setRunning(false);
    };
    term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(data));
    });
    term.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols, rows }));
    });
    const onWinResize = () => fit.fit();
    window.addEventListener("resize", onWinResize);
    winResizeRef.current = onWinResize;
  }

  useEffect(() => {
    if (termRef.current) termRef.current.options.theme = TERM_THEME[theme];
  }, [theme]);

  useEffect(() => () => { stop(); termRef.current?.dispose(); }, []);

  return (
    <div className="space-y-3">
      <div className="flex gap-2">
        <input
          value={cmd}
          onChange={(e) => setCmd(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && start(cmd)}
          className={`${control} flex-1 font-mono`}
          placeholder="/bin/sh"
        />
        <button onClick={() => start(cmd)}
          className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover">
          {running ? "Restart" : "Connect"}
        </button>
        {running && (
          <button onClick={stop} className="rounded-md border border-edge px-3 py-1.5 text-sm text-ink hover:bg-fog-800">
            Disconnect
          </button>
        )}
      </div>
      <div className="flex gap-2 text-xs">
        {["/bin/sh", "bash", "bundle exec rails console", "bin/rails dbconsole"].map((c) => (
          <button key={c} onClick={() => { setCmd(c); start(c); }}
            className="rounded border border-hair px-2 py-0.5 text-ink-muted hover:text-ink">
            {c}
          </button>
        ))}
      </div>
      <div
        ref={containerRef}
        className="h-[28rem] overflow-hidden rounded-lg border border-hair p-1"
        style={{ background: TERM_THEME[theme].background }}
      />
    </div>
  );
}
