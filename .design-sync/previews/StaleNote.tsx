import { StaleNote } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// Under numbers that are still on screen while the poll behind them fails.
export function UnderMetrics() {
  return (
    <Surface className="max-w-sm">
      <div className="grid grid-cols-3 gap-4">
        {[["CPU", "12%"], ["Memory", "5.1 GB"], ["Disk", "61%"]].map(([k, v]) => (
          <div key={k}>
            <p className="caps-label">{k}</p>
            <p className="text-lg font-semibold tabular-nums text-ink">{v}</p>
          </div>
        ))}
      </div>
      <StaleNote what="these numbers" />
    </Surface>
  );
}
