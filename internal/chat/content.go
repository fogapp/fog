package chat

import (
	"errors"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// Content is fog's profile of the MIMI content model (draft-ietf-mimi-
// content): what a message *is*, once decrypted. It is encoded as a CBOR map
// with string keys and lives only inside an MLS PrivateMessage — the server
// never sees one. The SDK carries the same shape in TypeScript.
type Content struct {
	// V is the profile version.
	V int `cbor:"v"`
	// Type: message | edit | delete | reaction | pin | receipt | room |
	// typing | history. (The SDK's "membership" is a timeline line a client
	// derives from an MLS commit; it is never encoded.)
	Type string `cbor:"type"`
	// ID is the message id — the same ULID as the event carrying it.
	ID     string `cbor:"id,omitempty"`
	SentAt string `cbor:"sentAt,omitempty"`
	// Body is the text (message, edit).
	Body *Body `cbor:"body,omitempty"`
	// TopicID groups a thread: the root message's id.
	TopicID string `cbor:"topicId,omitempty"`
	// InReplyTo quotes a message.
	InReplyTo string `cbor:"inReplyTo,omitempty"`
	// Replaces names the message an edit or delete acts on.
	Replaces string `cbor:"replaces,omitempty"`
	// Reaction (type reaction).
	Reaction *Reaction `cbor:"reaction,omitempty"`
	// Pin (type pin).
	Pin *Pin `cbor:"pin,omitempty"`
	// Mentions are person addresses named in the body.
	Mentions []string `cbor:"mentions,omitempty"`
	// Attachments are encrypted blobs the body refers to.
	Attachments []Attachment `cbor:"attachments,omitempty"`
	// Expires is when receivers should delete the message.
	Expires string `cbor:"expires,omitempty"`
	// Room carries encrypted room metadata (type room).
	Room *RoomMeta `cbor:"room,omitempty"`
	// Receipt (type receipt): the seq the sender has read up to.
	ReadSeq int64 `cbor:"readSeq,omitempty"`
	// System marks a message fog itself composed (the bot).
	System string `cbor:"system,omitempty"`
	// History (type history) carries earlier messages re-sent for devices
	// that joined after them, in a room whose policy shares history. Only the
	// devices named in HistoryFor act on it; everyone else ignores it.
	History    []HistoryItem `cbor:"history,omitempty"`
	HistoryFor []string      `cbor:"historyFor,omitempty"`
	// Preview is a link card the sender's device fetched and encrypted in.
	Preview *PreviewCard `cbor:"preview,omitempty"`
}

// PreviewCard is what a sender chose to attach about a link.
type PreviewCard struct {
	URL         string `cbor:"url"`
	Title       string `cbor:"title,omitempty"`
	Description string `cbor:"description,omitempty"`
	Image       string `cbor:"image,omitempty"`
	Site        string `cbor:"site,omitempty"`
}

// HistoryItem is one earlier message as its sharer held it.
type HistoryItem struct {
	ID      string  `cbor:"id"`
	Seq     int64   `cbor:"seq"`
	Sender  string  `cbor:"sender"`
	SentAt  string  `cbor:"sentAt,omitempty"`
	HubAt   string  `cbor:"hubAt,omitempty"`
	Content Content `cbor:"content"`
}

type Body struct {
	Format string `cbor:"format"` // text/markdown
	Text   string `cbor:"text"`
}

type Reaction struct {
	Target string `cbor:"target"`
	Emoji  string `cbor:"emoji"`
	Add    bool   `cbor:"add"`
}

type Pin struct {
	Target string `cbor:"target"`
	Pinned bool   `cbor:"pinned"`
}

type Attachment struct {
	Blob   string `cbor:"blob"` // sha256 of the ciphertext
	Key    []byte `cbor:"key"`
	Nonce  []byte `cbor:"nonce"`
	Name   string `cbor:"name,omitempty"`
	Mime   string `cbor:"mime,omitempty"`
	Size   int64  `cbor:"size,omitempty"`
	Width  int    `cbor:"w,omitempty"`
	Height int    `cbor:"h,omitempty"`
	// Thumb is a small encrypted preview, inline.
	Thumb []byte `cbor:"thumb,omitempty"`
}

type RoomMeta struct {
	Name   string `cbor:"name,omitempty"`
	Topic  string `cbor:"topic,omitempty"`
	Avatar []byte `cbor:"avatar,omitempty"`
}

var contentEnc, _ = cbor.CanonicalEncOptions().EncMode()

// EncodeContent serializes content canonically.
func EncodeContent(c Content) ([]byte, error) {
	if c.V == 0 {
		c.V = 1
	}
	return contentEnc.Marshal(c)
}

// DecodeContent parses content, refusing unknown profile versions.
func DecodeContent(b []byte) (Content, error) {
	var c Content
	if err := cbor.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.V != 1 {
		return c, errors.New("unknown content version")
	}
	if !wireTypes[c.Type] {
		return c, errors.New("content type no member may send")
	}
	return c, nil
}

// wireTypes are the content types a member may send. "membership" is a line
// a client derives from a commit it verified, never content.
var wireTypes = map[string]bool{"message": true, "edit": true, "delete": true, "reaction": true, "pin": true, "receipt": true, "room": true, "typing": true, "history": true}

// TextMessage builds a plain markdown message.
func TextMessage(id, text string) Content {
	return Content{V: 1, Type: "message", ID: id, SentAt: time.Now().UTC().Format(time.RFC3339), Body: &Body{Format: "text/markdown", Text: text}}
}
