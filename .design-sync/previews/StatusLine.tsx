import { StatusLine } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

export function Tones() {
  return (
    <Surface className="space-y-2">
      <StatusLine tone="ok">Mounted from 192.168.1.111/media · 3.2 TB free</StatusLine>
      <StatusLine tone="warn">Last offsite copy failed — retrying tonight</StatusLine>
      <StatusLine>Not set up</StatusLine>
    </Surface>
  );
}
