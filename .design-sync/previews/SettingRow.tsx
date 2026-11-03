import { SettingRow, SettingRows, Toggle } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// A list of preferences: words on the left, the switch on the right.
export function Rows() {
  return (
    <Surface>
      <SettingRows>
        <SettingRow label="A peer's fleet breaks" hint="Push when a peer that sends its errors here reports one.">
          <Toggle checked onChange={() => {}} aria-label="Push when a peer's fleet breaks" />
        </SettingRow>
        <SettingRow label="Snapshot before updates" hint="Applies to apps with backups on." error="Couldn't save the preference.">
          <Toggle checked={false} onChange={() => {}} aria-label="Snapshot before updates" />
        </SettingRow>
      </SettingRows>
    </Surface>
  );
}
