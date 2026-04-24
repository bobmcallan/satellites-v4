package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// TopicPrefix is the workspace-scoped topic prefix. Topics take the form
// "ws:<workspace_id>" so the topic string itself encodes the tenancy
// boundary — the AuthHub compares the suffix against event.WorkspaceID on
// every publish as a defence-in-depth check.
const TopicPrefix = "ws:"

// ErrInvalidTopic is returned when a caller supplies a topic that does not
// carry the required "ws:" prefix.
var ErrInvalidTopic = errors.New("hub: topic must be ws:<workspace_id>")

// ErrNotMember is returned when a caller subscribes to a workspace topic
// it is not a member of.
var ErrNotMember = errors.New("hub: caller is not a member of the target workspace")

// Membership is what AuthHub needs from the workspace layer to enforce
// the subscribe-time gate. Satisfied by workspace.Store's IsMember method.
type Membership interface {
	IsMember(ctx context.Context, workspaceID, userID string) (bool, error)
}

// MismatchAudit receives publish-time guard violations — events whose
// WorkspaceID does not match the topic's workspace suffix. The audit sink
// is injected (typically ledger-backed) so the hub package does not depend
// on the ledger store directly. A nil audit is treated as "silent drop".
type MismatchAudit interface {
	HubMismatch(ctx context.Context, event Event, expectedWorkspaceID string)
}

// AuthHub layers workspace-scoped auth over the raw Hub primitive. It
// gates Subscribe on workspace membership and drops Publish calls whose
// event.WorkspaceID does not match the topic's workspace suffix, routing
// drops to an optional MismatchAudit for persistence.
type AuthHub struct {
	hub    *Hub
	member Membership
	audit  MismatchAudit
}

// NewAuthHub wraps h with the membership gate and mismatch audit. audit
// may be nil — mismatched publishes are then dropped silently.
func NewAuthHub(h *Hub, m Membership, a MismatchAudit) *AuthHub {
	return &AuthHub{hub: h, member: m, audit: a}
}

// Hub returns the underlying raw hub for callers that need to publish
// workspace-stamped events from trusted server-side code. Most callers
// should use AuthHub.Publish instead so the defence-in-depth check runs.
func (a *AuthHub) Hub() *Hub {
	return a.hub
}

// Subscribe gates a Subscribe call on the caller's workspace membership.
// Topic must be "ws:<workspace_id>"; userID is expected to have been
// resolved by upstream auth middleware. Errors surface ErrInvalidTopic or
// ErrNotMember before any channel is allocated on the hub.
func (a *AuthHub) Subscribe(ctx context.Context, topic, subscriberID, userID string) (<-chan Event, error) {
	wsID, err := ParseTopicWorkspace(topic)
	if err != nil {
		return nil, err
	}
	if err := a.requireMember(ctx, wsID, userID); err != nil {
		return nil, err
	}
	return a.hub.Subscribe(topic, subscriberID), nil
}

// SubscribeSince is Subscribe with an initial replay of events whose ID
// is greater than sinceID. Membership enforcement is identical to
// Subscribe; a non-member receives no replay.
func (a *AuthHub) SubscribeSince(ctx context.Context, topic, subscriberID, userID, sinceID string) (<-chan Event, error) {
	wsID, err := ParseTopicWorkspace(topic)
	if err != nil {
		return nil, err
	}
	if err := a.requireMember(ctx, wsID, userID); err != nil {
		return nil, err
	}
	return a.hub.SubscribeSince(topic, subscriberID, sinceID), nil
}

// Unsubscribe removes subscriberID from its topic on the underlying hub.
// Safe to call multiple times; mirrors Hub.Unsubscribe.
func (a *AuthHub) Unsubscribe(subscriberID string) {
	a.hub.Unsubscribe(subscriberID)
}

// Publish asserts event.WorkspaceID equals the topic's workspace suffix
// before delegating to the underlying hub. A mismatch routes the event to
// the injected MismatchAudit (if any) and drops the publish — no event
// reaches subscribers, nothing is added to the replay buffer.
func (a *AuthHub) Publish(ctx context.Context, topic string, event Event) {
	wsID, err := ParseTopicWorkspace(topic)
	if err != nil {
		if a.audit != nil {
			a.audit.HubMismatch(ctx, event, "")
		}
		return
	}
	if event.WorkspaceID != wsID {
		if a.audit != nil {
			a.audit.HubMismatch(ctx, event, wsID)
		}
		return
	}
	a.hub.Publish(topic, event)
}

// ReplayBuffer exposes the underlying ring for the same topic. Membership
// is enforced at Subscribe time; ReplayBuffer is used by trusted
// server-side code paths (e.g. catch-up on reconnect after re-auth) and
// carries no membership check of its own.
func (a *AuthHub) ReplayBuffer(topic, sinceID string) []Event {
	return a.hub.ReplayBuffer(topic, sinceID)
}

func (a *AuthHub) requireMember(ctx context.Context, workspaceID, userID string) error {
	if a.member == nil {
		return fmt.Errorf("hub: membership store not configured")
	}
	ok, err := a.member.IsMember(ctx, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("hub: membership check: %w", err)
	}
	if !ok {
		return ErrNotMember
	}
	return nil
}

// ParseTopicWorkspace returns the workspace id encoded as the suffix of a
// "ws:<workspace_id>" topic. ErrInvalidTopic is returned when the prefix
// is absent or the suffix is empty.
func ParseTopicWorkspace(topic string) (string, error) {
	if !strings.HasPrefix(topic, TopicPrefix) {
		return "", ErrInvalidTopic
	}
	suffix := strings.TrimPrefix(topic, TopicPrefix)
	if suffix == "" {
		return "", ErrInvalidTopic
	}
	return suffix, nil
}
