// Pacing for what the client sends unprompted: typing signals and stream
// reconnects. Neither may be left to the UI's good manners — a native client
// without a throttle would send a typing signal per keystroke, and every
// device reconnecting on the same fixed beat after a fogd restart is a
// thundering herd.

// Throttle lets one call per key through each interval.
export class Throttle {
  private last = new Map<string, number>();
  private ms: number;
  private now: () => number;
  constructor(ms: number, now: () => number = Date.now) {
    this.ms = ms;
    this.now = now;
  }

  ready(key: string): boolean {
    const t = this.now();
    const prev = this.last.get(key);
    if (prev !== undefined && t - prev < this.ms) return false;
    this.last.set(key, t);
    return true;
  }
}

// reconnectDelay: how long to wait before reconnect attempt n (0 for the
// first): doubling from one second to half a minute, each wait drawn from
// its upper half so clients that dropped together come back apart.
export function reconnectDelay(attempt: number, random: () => number = Math.random): number {
  const ceiling = Math.min(30_000, 1000 * 2 ** Math.min(attempt, 15));
  return Math.round(ceiling / 2 + (ceiling / 2) * random());
}
