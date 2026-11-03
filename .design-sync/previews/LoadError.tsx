import { LoadError } from "fog-frontend";
import { Surface } from "../preview-lib/surface";

// A fetch that never landed: distinct from an empty result, with the retry.
export function WithDetail() {
  return (
    <Surface className="max-w-md">
      <LoadError label="Couldn't load apps." error={new Error("fetch failed: 502 Bad Gateway from fogd")} onRetry={() => {}} />
    </Surface>
  );
}

export function WithoutDetail() {
  return (
    <Surface className="max-w-md">
      <LoadError label="Couldn't reach the NAS." onRetry={() => {}} />
    </Surface>
  );
}

export function NoRetry() {
  return (
    <Surface className="max-w-md">
      <LoadError label="This kit can't be opened." error="archive is encrypted with a key fog doesn't hold" />
    </Surface>
  );
}
