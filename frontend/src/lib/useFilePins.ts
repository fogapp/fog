// Drive pins: the files and folders a person keeps under Drive in the
// sidebar. The key sits under filesKey, so every Drive verb that settles
// (rename, move, trash, restore) re-reads the pins too — a renamed folder's
// pin follows it, a trashed one shows as missing, without either side
// having to know about the other.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { del, get, post, type FilePin } from "../api";
import { filesKey } from "./useFiles";

const filePinsKey = [...filesKey, "pins"] as const;

export function useFilePins(enabled = true) {
  return useQuery({
    queryKey: filePinsKey,
    queryFn: () => get<{ pins: FilePin[] }>("/api/files/pins").then((r) => r.pins),
    enabled,
    staleTime: 30_000,
    refetchInterval: 60_000,
  });
}

export function usePinActions() {
  const qc = useQueryClient();
  const settle = () => qc.invalidateQueries({ queryKey: filePinsKey });
  const pin = useMutation({
    mutationFn: (path: string) => post("/api/files/pins", { path }),
    onSettled: settle,
  });
  const unpin = useMutation({
    mutationFn: (path: string) => del(`/api/files/pins?path=${encodeURIComponent(path)}`),
    onSettled: settle,
  });
  return { pin, unpin };
}
