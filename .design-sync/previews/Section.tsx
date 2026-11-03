import { Button, Field, Section } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// One block of a settings page: an anchored heading, one line on what it
// is, then the controls.
export function Basic() {
  return (
    <Surface>
      <Section id="hostname" title="Hostname" description="What this fog answers to on your network.">
        <div className="max-w-sm">
          <Field label="Hostname" defaultValue="fog" hint="Apps are reachable as <app>.fog.lan." />
        </div>
      </Section>
    </Surface>
  );
}

export function WithActions() {
  return (
    <Surface>
      <Section
        id="offsite"
        title="Offsite copy"
        description="A second copy of every kit, in a bucket you control."
        actions={<Button size="sm">Run now</Button>}
      >
        <p className="text-sm text-ink-muted">Last copy finished 02:14 today · 41 GB in the bucket.</p>
      </Section>
    </Surface>
  );
}
