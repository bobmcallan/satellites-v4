package client

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/bobmcallan/satellites/internal/session"
)

// SessionWhoamiInput names the session being queried. The caller
// resolves the session id from request headers + body args before
// invoking the typed method.
type SessionWhoamiInput struct {
	SessionID string
}

// SessionWhoamiOutput mirrors the wire payload of the session_whoami
// verb. Empty WorkspaceID / ActiveProjectID fields are omitted at the
// wire boundary by the caller.
type SessionWhoamiOutput struct {
	UserID          string    `json:"user_id"`
	SessionID       string    `json:"session_id"`
	Source          string    `json:"source"`
	Registered      time.Time `json:"registered_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	WorkspaceID     string    `json:"workspace_id,omitempty"`
	ActiveProjectID string    `json:"active_project_id,omitempty"`
}

// ErrSessionIDRequired is returned by SessionWhoami when the input
// supplies no session id. The wire-layer caller maps this to a
// session_id_required error envelope.
var ErrSessionIDRequired = errors.New("session_id_required")

// ErrSessionNotRegistered is returned by SessionWhoami when the
// session id is not in the registry under the calling user.
var ErrSessionNotRegistered = errors.New("session_not_registered")

// SessionWhoami returns the caller's registered session row. Returns
// ErrSessionIDRequired when no session id is supplied;
// ErrSessionNotRegistered when the session is not in the registry.
func (c *Client) SessionWhoami(ctx context.Context, caller Caller, in SessionWhoamiInput) (SessionWhoamiOutput, error) {
	if in.SessionID == "" {
		return SessionWhoamiOutput{}, ErrSessionIDRequired
	}
	if c.deps.Sessions == nil {
		return SessionWhoamiOutput{}, ErrSessionNotRegistered
	}
	sess, err := c.deps.Sessions.Get(ctx, caller.UserID, in.SessionID)
	if err != nil {
		return SessionWhoamiOutput{}, ErrSessionNotRegistered
	}
	return sessionRowToWhoamiOutput(sess), nil
}

// SessionRegisterInput is the input for SessionRegister. SessionID
// may be empty — when ProjectID is supplied the typed method first
// attempts session resume (most-recent fresh session for the same
// user+project); otherwise a fresh uuid is minted.
type SessionRegisterInput struct {
	SessionID   string
	Source      string
	WorkspaceID string
	ProjectID   string
	Now         time.Time
	Staleness   time.Duration
}

// SessionRegisterOutput mirrors the wire payload of session_register.
type SessionRegisterOutput struct {
	UserID          string    `json:"user_id"`
	SessionID       string    `json:"session_id"`
	Source          string    `json:"source"`
	Registered      time.Time `json:"registered_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	Resumed         bool      `json:"resumed"`
	WorkspaceID     string    `json:"workspace_id,omitempty"`
	ActiveProjectID string    `json:"active_project_id,omitempty"`
}

// SessionRegister implements session_register's business logic:
// resume-by-project when applicable, mint-new otherwise; persist
// optional workspace + active project bindings.
func (c *Client) SessionRegister(ctx context.Context, caller Caller, in SessionRegisterInput) (SessionRegisterOutput, error) {
	if c.deps.Sessions == nil {
		return SessionRegisterOutput{}, errors.New("session store not configured")
	}
	source := in.Source
	if source == "" {
		source = session.SourceSessionStart
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Session-resume semantics (story_cef068fe): when no explicit
	// session id is carried but a project_id is, look up the most
	// recent active (non-stale) session for (user, project_id).
	resumed := false
	sessionID := in.SessionID
	if sessionID == "" && in.ProjectID != "" {
		if prior, ok := c.findFreshSessionForProject(ctx, caller.UserID, in.ProjectID, now, in.Staleness); ok {
			sessionID = prior.SessionID
			resumed = true
		}
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	sess, err := c.deps.Sessions.Register(ctx, caller.UserID, sessionID, source, now)
	if err != nil {
		return SessionRegisterOutput{}, err
	}
	if in.WorkspaceID != "" {
		if updated, err := c.deps.Sessions.SetWorkspace(ctx, caller.UserID, sessionID, in.WorkspaceID, now); err == nil {
			sess = updated
		}
	}
	if in.ProjectID != "" {
		if updated, err := c.deps.Sessions.SetActiveProject(ctx, caller.UserID, sessionID, in.ProjectID, now); err == nil {
			sess = updated
		}
	}
	out := SessionRegisterOutput{
		UserID:          sess.UserID,
		SessionID:       sess.SessionID,
		Source:          sess.Source,
		Registered:      sess.Registered,
		LastSeenAt:      sess.LastSeenAt,
		Resumed:         resumed,
		WorkspaceID:     sess.WorkspaceID,
		ActiveProjectID: sess.ActiveProjectID,
	}
	return out, nil
}

// findFreshSessionForProject mirrors mcpserver.Server.findFreshSessionForProject:
// the most recent non-stale session bound to the given (user, project).
// Returns ok=false when nothing matches; the caller mints a fresh id.
func (c *Client) findFreshSessionForProject(ctx context.Context, userID, projectID string, now time.Time, staleness time.Duration) (session.Session, bool) {
	if c.deps.Sessions == nil {
		return session.Session{}, false
	}
	rows, err := c.deps.Sessions.ListAll(ctx)
	if err != nil {
		return session.Session{}, false
	}
	if staleness <= 0 {
		staleness = session.StalenessDefault
	}
	var best session.Session
	found := false
	for _, row := range rows {
		if row.UserID != userID || row.ActiveProjectID != projectID {
			continue
		}
		if session.IsStale(row, now, staleness) {
			continue
		}
		if !found || row.LastSeenAt.After(best.LastSeenAt) {
			best = row
			found = true
		}
	}
	return best, found
}

// sessionRowToWhoamiOutput projects a session.Session into the wire
// payload shape. Shared between SessionWhoami and tests.
func sessionRowToWhoamiOutput(s session.Session) SessionWhoamiOutput {
	return SessionWhoamiOutput{
		UserID:          s.UserID,
		SessionID:       s.SessionID,
		Source:          s.Source,
		Registered:      s.Registered,
		LastSeenAt:      s.LastSeenAt,
		WorkspaceID:     s.WorkspaceID,
		ActiveProjectID: s.ActiveProjectID,
	}
}
