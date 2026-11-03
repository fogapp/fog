// A thread: the root message and its replies in a side pane, with its own
// composer that posts with the root's id as topicId.

import type { ChatClient, DirectoryPerson, ParticipantView, Room } from "@fog/chat-sdk";
import Composer from "./Composer";
import Timeline from "./Timeline";

export default function ThreadPane({
  client, room, topicId, participants, directory, onClose,
}: {
  client: ChatClient;
  room: Room;
  topicId: string;
  participants: ParticipantView[];
  directory: DirectoryPerson[];
  onClose: () => void;
}) {
  const messages = client.messages(room.id, { topicId });
  return (
    <div className="flex h-full flex-col">
      <div className="flex h-11 shrink-0 items-center justify-between border-b border-hair px-4">
        <span className="text-sm font-medium text-ink">Thread</span>
        <button onClick={onClose} className="rounded px-1 text-ink-muted hover:text-ink" title="Close thread" aria-label="Close thread">
          ✕
        </button>
      </div>
      {messages.length === 0 ? (
        <p className="p-4 text-hint text-ink-muted">This thread's root is not in this device's store.</p>
      ) : (
        <Timeline
          client={client}
          roomId={room.id}
          sync={room.sync}
          canModerate={(room.myRole ?? 0) >= 3}
          directory={directory}
          messages={messages}
          threadMode
          emptyLabel="No replies yet."
        />
      )}
      <Composer key={`${room.id}/${topicId}`} client={client} roomId={room.id} participants={participants} directory={directory} topicId={topicId} placeholder="Reply in thread" />
    </div>
  );
}
