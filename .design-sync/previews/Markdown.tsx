import { Markdown } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// App notes as they arrive from a catalog entry or fog.yaml: headings, a
// list, inline code, a link. Every element style lives in the component.
export function AppNotes() {
  return (
    <Surface className="max-w-lg">
      <Markdown>{`## First run

Jellyfin starts on port **8096**. Point a library at \`/media\` — that is the NAS share fog mounted for it.

- Hardware transcoding needs the \`render\` group; fog adds it.
- Metadata is fetched from [TheMovieDB](https://www.themoviedb.org) on first scan.

> Backups cover the config volume only. Media is already on the NAS.`}</Markdown>
    </Surface>
  );
}

export function CodeBlock() {
  return (
    <Surface className="max-w-lg">
      <Markdown>{`### Manual restore

Stop the app, then copy the kit back into the volume:

\`\`\`
fog app stop jellyfin
fog restore jellyfin --from kit/2026-09-01
fog app start jellyfin
\`\`\`

1. Check \`fog app logs jellyfin\` for the first-boot migration.
2. Open the app once so the library rescans.`}</Markdown>
    </Surface>
  );
}
