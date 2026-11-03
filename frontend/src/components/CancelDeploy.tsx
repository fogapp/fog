// The way out of a deploy, update, or rollback that has hung — a snapshot
// the app never finishes, a build that stalls. Cancelling only signals: the
// operation unwinds on the server and the progress panel keeps reading as
// running until it has, so this button claims nothing beyond "asked". The
// server's own stage ("cancelling") is what says the request landed.

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { post } from "../api";
import Button from "./Button";
import { deployStatusKey } from "../lib/useAppData";

export default function CancelDeploy({ name, stage }: { name: string; stage?: string }) {
  const qc = useQueryClient();
  const cancel = useMutation({
    mutationFn: () => post(`/api/apps/${name}/deploy/cancel`),
    onSettled: () => qc.invalidateQueries({ queryKey: deployStatusKey(name) }),
  });
  const cancelling = stage === "cancelling";
  return (
    <span className="inline-flex items-center gap-2">
      {cancel.isError && <span className="text-xs text-warn">{cancel.error.message}</span>}
      <Button size="sm" variant="danger" disabled={cancel.isPending || cancelling}
        onClick={() => cancel.mutate()}>
        Cancel
      </Button>
    </span>
  );
}
