// Package notify sends operator notifications. ntfy first — it's the
// self-hosted-friendly choice; more channels can hang off the same interface.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/appevents"
	"gitea.lan/matthew/fog/internal/store"
)

const (
	SettingNtfyServer = "ntfy_server" // e.g. https://ntfy.sh
	SettingNtfyTopic  = "ntfy_topic"
)

type Notifier struct {
	Store *store.Store
	Log   *slog.Logger
	// Events, when set, also receives every notification as a fog.notice
	// event (docs/events-contract.md), ntfy configured or not — so a
	// consumer like HUD can take over routing fog's own alerts.
	Events *appevents.Bus
}

// NoticeEvent is the type of the event every Send publishes.
const NoticeEvent = "fog.notice"

// Send fires a notification; no-op unless ntfy is configured. Failures are
// logged, never fatal — notifications must not break operations. Sends
// retry briefly: the moments a notification matters most (the network just
// changed, the server just booted in a new house) are exactly the moments
// one attempt is likeliest to fail.
func (n *Notifier) Send(title, message string, ok bool) {
	n.publish(title, message, ok)
	server, _ := n.Store.GetSetting(SettingNtfyServer)
	topic, _ := n.Store.GetSetting(SettingNtfyTopic)
	if server == "" || topic == "" {
		return
	}
	go n.send(context.Background(), server, topic, title, message, ok)
}

// publish puts the notification on the event stream. level is the one
// thing ntfy's tags carried: whether this is good news or needs a look.
func (n *Notifier) publish(title, message string, ok bool) {
	if n.Events == nil {
		return
	}
	level := "alert"
	if ok {
		level = "ok"
	}
	if err := n.Events.PublishFog(NoticeEvent, title, map[string]any{"title": title, "body": message, "level": level}); err != nil {
		n.Log.Warn("publish notice event", "title", title, "err", err)
	}
}

// SendTo fires a notification at a specific topic on the configured server —
// a person's own topic for chat, rather than the operator's. No-op without a
// server.
func (n *Notifier) SendTo(topic, title, message string) {
	server, _ := n.Store.GetSetting(SettingNtfyServer)
	if server == "" || topic == "" {
		return
	}
	go n.send(context.Background(), server, topic, title, message, true)
}

// ErrNotConfigured is what SendNow answers when there is nowhere to send.
var ErrNotConfigured = errors.New("ntfy is not configured — set a server and topic first")

// SendNow delivers one notification and waits for the answer: the "Send
// test" button needs to say whether the message arrived, which the
// fire-and-forget Send deliberately never does. The retries can run to the
// better part of a minute, so ctx (the request) bounds them: a person who
// gives up and leaves takes the wait with them.
func (n *Notifier) SendNow(ctx context.Context, title, message string, ok bool) error {
	server, _ := n.Store.GetSetting(SettingNtfyServer)
	topic, _ := n.Store.GetSetting(SettingNtfyTopic)
	if server == "" || topic == "" {
		return ErrNotConfigured
	}
	return n.send(ctx, server, topic, title, message, ok)
}

// topicRe is ntfy's own topic grammar. A topic is one path segment and
// nothing else: a household member chooses their own, and one carrying "?"
// or "%2F" would aim fog's POST at another path or query on the
// household's ntfy server — ?email= and ?call= among what ntfy acts on.
var topicRe = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

// ValidTopic reports whether t is a topic ntfy accepts. Empty is not.
func ValidTopic(t string) bool { return topicRe.MatchString(t) }

// ValidServer reports why a server setting is unusable, or nil: an http or
// https URL with a host and nothing after its path. Credentials in the URL
// are allowed — that is how a self-hosted, auth-required ntfy is reached.
func ValidServer(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("the ntfy server must be an http:// or https:// URL, e.g. https://ntfy.sh")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("the ntfy server URL must not carry a query or fragment")
	}
	return nil
}

// topicURL is where one notification is POSTed: the server's URL with the
// topic as one more path segment, never spliced in as text.
func topicURL(server, topic string) (string, error) {
	if err := ValidServer(server); err != nil {
		return "", err
	}
	if !ValidTopic(topic) {
		return "", errors.New("the ntfy topic may hold only letters, digits, - and _ (at most 64)")
	}
	u, _ := url.Parse(server)
	return u.JoinPath(topic).String(), nil
}

// headerSafe flattens what fog puts in a header — a title can carry a room
// name someone typed — to one line of printable text.
func headerSafe(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, v)
}

// client sends notifications and follows no redirect. The server is the
// operator's choice; where it points next is not — a public ntfy, or
// anything on the path to a plain-http one, could answer 307 with a LAN
// address and have fog POST there from inside the house.
var client = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// send delivers one notification, retrying transient failures. Only 2xx is
// success: a 403/404 (wrong topic, auth-required server — the typical setup
// misconfiguration) would otherwise drop every notification silently and
// forever, so it logs loudly and stops; 429 retries like a 5xx. Once ctx
// ends it stops, mid-request or between attempts.
func (n *Notifier) send(ctx context.Context, server, topic, title, message string, ok bool) error {
	target, err := topicURL(server, topic)
	if err != nil {
		// Never fixed by a retry; and a topic is a bearer secret on a public
		// server, so the log says which setting, not its value.
		n.Log.Warn("ntfy not sent", "title", title, "err", err)
		return err
	}
	var lastErr error
	for attempt, delay := 0, 5*time.Second; attempt < 3; attempt, delay = attempt+1, delay*5 {
		if attempt > 0 {
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
		// A fresh request per attempt: the body reader is consumed by a send.
		req, err := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(message))
		if err != nil {
			return err
		}
		req.Header.Set("Title", headerSafe(title))
		if ok {
			req.Header.Set("Tags", "white_check_mark")
		} else {
			req.Header.Set("Tags", "rotating_light")
			req.Header.Set("Priority", "high")
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("ntfy answered HTTP %d", resp.StatusCode)
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				lastErr = fmt.Errorf("ntfy answered HTTP %d (a redirect, which fog does not follow) — set the server to the address it redirects to", resp.StatusCode)
			}
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
				continue // transient: the server may recover before the next attempt
			}
			// Any other status will fail identically on retry — say why once.
			n.Log.Warn("ntfy rejected notification", "status", resp.StatusCode, "title", title)
			return lastErr
		}
		lastErr = err
	}
	// A transport error quotes the URL, topic and all; log the cause only.
	logged := lastErr
	var ue *url.Error
	if errors.As(lastErr, &ue) {
		logged = ue.Err
	}
	n.Log.Warn("ntfy send failed after retries", "title", title, "err", logged)
	return lastErr
}
