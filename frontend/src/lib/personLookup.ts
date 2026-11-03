// A person, looked up by person address or chat address: who they are, how
// to reach them in chat, and where apps say they live
// (docs/signin-contract.md). Shared by /chat/with/<address> and the room info
// pane.
import { useQuery } from "@tanstack/react-query";
import { get } from "../api";

interface PersonPage {
  app: string;
  url: string;
  label: string;
}

export interface PersonLookup {
  address: string;
  handle: string;
  displayName: string;
  ours?: boolean;
  household?: string;
  chatAddress: string;
  pages: PersonPage[];
  sharedRooms: { id: string; kind: "channel" | "group" | "dm"; label: string }[];
}

export function usePersonLookup(by: { address?: string; chat?: string }) {
  const params = new URLSearchParams();
  if (by.address) params.set("address", by.address);
  if (by.chat) params.set("chat", by.chat);
  return useQuery({
    queryKey: ["person-lookup", by.address ?? "", by.chat ?? ""],
    queryFn: () => get<PersonLookup>(`/api/people/lookup?${params}`),
    enabled: !!(by.address || by.chat),
    retry: false,
    staleTime: 60_000,
  });
}
