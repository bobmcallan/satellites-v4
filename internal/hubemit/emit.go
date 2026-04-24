// Package hubemit is the narrow emit surface stores import so they can
// publish workspace-scoped events without a direct dependency on the
// internal/hub package.
//
// A concrete Publisher (typically backed by *hub.AuthHub at boot) is
// injected via each store's SetPublisher method. Tests substitute a
// recording stub to assert emit points fire with the expected kind and
// payload.
package hubemit

import "context"

// Publisher is the minimal emit contract. Stores call Publish at the
// tail of each mutation; implementations decide the actual fan-out.
type Publisher interface {
	Publish(ctx context.Context, topic, kind, workspaceID string, data any)
}

// NoopPublisher is the safe default: implements Publisher and drops
// every call. Stores can hold a NoopPublisher at zero value so hook
// code never needs a nil guard.
type NoopPublisher struct{}

// Publish implements Publisher.
func (NoopPublisher) Publish(context.Context, string, string, string, any) {}
