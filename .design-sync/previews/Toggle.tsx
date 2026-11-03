import { Toggle } from "fog-frontend";
import { Surface } from "../preview-lib/surface";
import { useState } from "react";

// On and off, side by side: the track tints to the beacon at 30% when on
// and the knob becomes the beacon; off is a quiet grey.
export function OnAndOff() {
  return (
    <Surface className="flex flex-wrap items-center gap-8">
      <Toggle checked onChange={() => {}} label="Follow" />
      <Toggle checked={false} onChange={() => {}} label="Reveal secrets" />
      <Toggle checked disabled onChange={() => {}} label="Locked while deploying" />
    </Surface>
  );
}

// Live: click to flip.
export function Interactive() {
  const [on, setOn] = useState(true);
  return (
    <Surface className="flex items-center gap-8">
      <Toggle checked={on} onChange={setOn} label={on ? "Following" : "Paused"} />
      <span className="text-xs text-ink-muted">click it</span>
    </Surface>
  );
}

// Where it lives in fog: the right slot of a section head, beside filter
// pills, as on the app page's Logs tab.
export function InSectionHead() {
  const [follow, setFollow] = useState(true);
  return (
    <Surface className="w-[520px]">
      <div className="flex items-center gap-3">
        <span className="field-label shrink-0">Log</span>
        <span aria-hidden className="h-0.5 w-3.5 shrink-0 rounded-full bg-accent" />
        <span aria-hidden className="h-px min-w-4 flex-1 bg-hair-strong" />
        <span className="inline-flex gap-1.5">
          <span className="rounded-full border border-accent/30 bg-accent/10 px-2.5 py-[3px] text-xs text-accent">All</span>
          <span className="rounded-full border border-veil-edge px-2.5 py-[3px] text-xs text-ink-muted">Warnings</span>
        </span>
        <Toggle checked={follow} onChange={setFollow} label="Follow" />
      </div>
    </Surface>
  );
}
