// Source primitives for the wshandler — extracted as part of the
// hub→surreallive cutover (sty_010a0543). The wshandler used to
// consume from *hub.AuthHub directly; with the hub gone, it consumes
// from an `EventSource` interface that the surreallive-backed
// implementation satisfies. Tests inject a stub.

package wshandler

import (
	"context"
	"errors"
	"strings"
)

// TopicPrefix is the workspace-scoped topic prefix. Topics take the
// form "ws:<workspace_id>" so the topic string itself encodes the
// tenancy boundary.
const TopicPrefix = "ws:"

// ErrInvalidTopic is returned when a caller supplies a topic that does
// not carry the required "ws:" prefix.
var ErrInvalidTopic = errors.New("wshandler: topic must be ws:<workspace_id>")

// ErrNotMember is returned when a caller subscribes to a workspace
// topic it is not a member of.
var ErrNotMember = errors.New("wshandler: caller is not a member of the target workspace")

// WireEvent is the JSON shape sent to the websocket client. Mirrors
// the legacy hub.Event field layout — portal + agent consumers depend
// on this exact shape.
type WireEvent struct {
	ID          string         `json:"ID"`
	Topic       string         `json:"Topic"`
	Kind        string         `json:"Kind"`
	WorkspaceID string         `json:"WorkspaceID"`
	CreatedAt   string         `json:"CreatedAt,omitempty"`
	Data        map[string]any `json:"Data,omitempty"`
}

// Membership is what an EventSource implementation needs from the
// workspace layer to enforce the subscribe-time gate. Satisfied by
// workspace.Store's IsMember method.
type Membership interface {
	IsMember(ctx context.Context, workspaceID, userID string) (bool, error)
}

// EventSource is the wshandler's upstream — workspace-scoped event
// stream with per-subscriber Subscribe / Unsubscribe semantics.
// Production: SurrealLiveSource. Tests: a stub.
type EventSource interface {
	// Subscribe registers subscriberID against topic and returns a
	// receive channel of events scoped to that workspace.
	// Implementations enforce membership via the Membership the
	// source was constructed with; non-members receive ErrNotMember.
	// Topics outside the ws:<workspace_id> shape return ErrInvalidTopic.
	//
	// projectID narrows delivery to events whose payload carries a
	// matching `project_id` key (the per-subscription gate added by
	// sty_fbcde932). Empty projectID preserves the legacy
	// workspace-only behaviour — every event on the topic is
	// delivered after the membership gate.
	Subscribe(ctx context.Context, topic, subscriberID, userID, projectID string) (<-chan WireEvent, error)
	// Unsubscribe releases subscriberID's channel. Safe to call on an
	// unknown id.
	Unsubscribe(subscriberID string)
}

// ParseTopicWorkspace returns the workspace id encoded as the suffix
// of a "ws:<workspace_id>" topic. ErrInvalidTopic is returned when
// the prefix is absent or the suffix is empty.
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
