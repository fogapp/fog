// "Ann is typing…" under the timeline. It listens for typing itself, so a
// ping — several a second in a busy room — redraws this one line and not
// the page around it.

import { useEffect, useState } from "react";
import type { ChatClient, DirectoryPerson } from "@fog/chat-sdk";
import { nameFor } from "./names";

export default function Typing({ client, roomId, directory }: { client: ChatClient; roomId: string; directory: DirectoryPerson[] }) {
  const [, setVersion] = useState(0);

  // The client sends a second ping as each one lapses, so the line also
  // clears on its own.
  useEffect(() => {
    const off = client.addEventListener("typing", (d) => {
      if ((d as { roomId?: string } | undefined)?.roomId === roomId) setVersion((v) => v + 1);
    });
    return () => {
      off();
    };
  }, [client, roomId]);

  const typing = client.typingIn(roomId).map((a) => nameFor(a, directory, client));
  return (
    <div className="h-5 px-6 text-hint text-ink-muted">
      {typing.length > 0 && `${typing.join(", ")} ${typing.length === 1 ? "is" : "are"} typing…`}
    </div>
  );
}
