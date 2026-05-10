// Deps wiring for the websocket surface: session → user resolution
// for wshandler. The pre-cutover hubPublisher + ledgerMismatchAudit
// adapters are gone with the hub (sty_010a0543).
package main

import (
	"context"
	"fmt"

	"github.com/bobmcallan/satellites/internal/auth"
)

// sessionResolverAdapter fans the wshandler.SessionResolver call out to
// the existing session + user stores.
type sessionResolverAdapter struct {
	sessions auth.SessionStore
	users    auth.UserStoreByID
}

// Resolve implements wshandler.SessionResolver.
func (a *sessionResolverAdapter) Resolve(_ context.Context, sessionID string) (auth.User, error) {
	sess, err := a.sessions.Get(sessionID)
	if err != nil {
		return auth.User{}, fmt.Errorf("session lookup: %w", err)
	}
	return a.users.GetByID(sess.UserID)
}
