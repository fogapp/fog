//go:build !linux

package netwatch

import (
	"context"
)

// No kernel subscription off Linux; the poller carries the watch alone.
func subscribeKernel(ctx context.Context, notify func(), live func(bool)) {}
