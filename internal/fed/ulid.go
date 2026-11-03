package fed

import "gitea.lan/matthew/fog/internal/randx"

// NewULID returns a fresh ULID (randx.ULID). The error is always nil; it
// stays for the callers that already check it.
func NewULID() (string, error) { return randx.ULID(), nil }
