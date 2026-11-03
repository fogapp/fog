// Chat push. The payload names the room, the sender, and whether this was a
// mention — never the text, which this server never had. Tapping the
// notification opens the room in an existing tab if there is one.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

self.addEventListener("push", (e) => {
  let p = {};
  try {
    p = e.data ? e.data.json() : {};
  } catch {
    p = {};
  }
  // fog telling a person about their own account (a password, a passkey,
  // a session changed): fog's own words, and a link into fog.
  if (p.kind === "account") {
    const url = typeof p.url === "string" && p.url.startsWith("/") && !p.url.startsWith("//") ? p.url : "/";
    e.waitUntil(
      self.registration.showNotification(p.title || "Your fog account changed", {
        body: p.body || "",
        tag: "fog-account",
        renotify: true,
        icon: "/fog-192.png",
        badge: "/fog-192.png",
        data: { url },
      }),
    );
    return;
  }
  const who = p.senderName || "Someone";
  const where = p.label ? ` in ${p.label}` : "";
  const title = p.kind === "mention" ? `${who} mentioned you${where}` : `${who}${where}`;
  const body = p.kind === "mention" ? "Open chat to read it." : "New message — open chat to read it.";
  e.waitUntil(
    self.registration.showNotification(title, {
      body,
      tag: p.roomId ? `fog-chat-${p.roomId}` : "fog-chat",
      renotify: true,
      icon: "/fog-192.png",
      badge: "/fog-192.png",
      data: { url: p.roomId ? `/chat/${encodeURIComponent(p.roomId)}` : "/chat" },
    }),
  );
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || "/chat";
  e.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((list) => {
      for (const c of list) {
        if (new URL(c.url).origin === self.location.origin && "focus" in c) {
          if ("navigate" in c) c.navigate(url);
          return c.focus();
        }
      }
      return self.clients.openWindow(url);
    }),
  );
});
