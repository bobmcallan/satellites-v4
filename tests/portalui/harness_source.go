//go:build portalui

// harnessSource is the test substitute for the production
// wshandler.SurrealLiveSource (sty_010a0543 cutover). The portalui
// chromedp suite drove events through the legacy AuthHub before that
// package was deleted; with the wshandler now consuming from
// wshandler.EventSource, the harness needs an in-process implementation
// that the tests can drive synchronously without standing up surrealdb.
//
// Design notes (sty_fa0cc6f3 plan):
//
//   - Subscribe mirrors SurrealLiveSource: ParseTopicWorkspace →
//     requireMember → register a buffered channel keyed by subscriberID
//     and topic. Re-registering a subscriberID drops the prior channel.
//   - Publish is the test-only fanout. Synchronous delivery under
//     s.mu so chromedp polls see deterministic ordering with no
//     goroutine-scheduling latency — critical for
//     TestStoryPanel_StatusPropagation_Under500ms's 500 ms budget.
//   - project_id gate on Subscribe matches production
//     SurrealLiveSource.fanout (sty_fbcde932): a non-empty entry.projectID
//     drops events whose payload's project_id key does not match.
//   - Drop-on-full + drop-on-mismatch parity with production keeps the
//     invariants the page-side code is written against.
//
// The stub is not a parallel reconstruction of the deleted hub
// (pr_substrate_model); it is the same EventSource shape production
// satisfies, with the surreallive notification feeder replaced by a
// test-driven Publish call.

package portalui

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bobmcallan/satellites/internal/wshandler"
)

// harnessChanBuf mirrors the production wshandler.SurrealLiveSource
// per-subscriber buffer size so a wedged test client surfaces the same
// drop-on-full signal pprod would.
const harnessChanBuf = 64

type harnessSubEntry struct {
	topic       string
	workspaceID string
	userID      string
	projectID   string
	ch          chan wshandler.WireEvent
}

// harnessSource implements wshandler.EventSource for the chromedp
// suite. Construct via newHarnessSource; the Harness wires it into
// wshandler.Deps and into PublishEvent / UpdateStoryStatus so tests
// fan events out through the same surface the production server reads
// from.
type harnessSource struct {
	members wshandler.Membership

	mu      sync.Mutex
	subs    map[string]*harnessSubEntry            // subID → entry
	byTopic map[string]map[string]*harnessSubEntry // topic → subID → entry

	idCounter atomic.Uint64
}

func newHarnessSource(members wshandler.Membership) *harnessSource {
	return &harnessSource{
		members: members,
		subs:    make(map[string]*harnessSubEntry),
		byTopic: make(map[string]map[string]*harnessSubEntry),
	}
}

// Subscribe implements wshandler.EventSource. Mirrors
// SurrealLiveSource.Subscribe.
func (s *harnessSource) Subscribe(ctx context.Context, topic, subscriberID, userID, projectID string) (<-chan wshandler.WireEvent, error) {
	wsID, err := wshandler.ParseTopicWorkspace(topic)
	if err != nil {
		return nil, err
	}
	if err := s.requireMember(ctx, wsID, userID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribeLocked(subscriberID)
	ch := make(chan wshandler.WireEvent, harnessChanBuf)
	entry := &harnessSubEntry{
		topic:       topic,
		workspaceID: wsID,
		userID:      userID,
		projectID:   projectID,
		ch:          ch,
	}
	s.subs[subscriberID] = entry
	if s.byTopic[topic] == nil {
		s.byTopic[topic] = make(map[string]*harnessSubEntry)
	}
	s.byTopic[topic][subscriberID] = entry
	return ch, nil
}

// Unsubscribe implements wshandler.EventSource. Idempotent on unknown
// ids.
func (s *harnessSource) Unsubscribe(subscriberID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribeLocked(subscriberID)
}

func (s *harnessSource) unsubscribeLocked(subscriberID string) {
	entry, ok := s.subs[subscriberID]
	if !ok {
		return
	}
	delete(s.subs, subscriberID)
	if subs, ok := s.byTopic[entry.topic]; ok {
		delete(subs, subscriberID)
		if len(subs) == 0 {
			delete(s.byTopic, entry.topic)
		}
	}
	close(entry.ch)
}

// Publish is the test-only fanout entry point. Synthesises a
// WireEvent on the ws:<workspaceID> topic and delivers it to every
// matching subscriber under s.mu. project_id-narrowed subscribers
// drop events whose payload's project_id does not match (same gate
// SurrealLiveSource.fanout enforces). Full channels drop the event
// (drop-on-full parity).
func (s *harnessSource) Publish(workspaceID, projectID, kind string, payload map[string]any) {
	if workspaceID == "" {
		return
	}
	// Mirror translate.go's project_id key so subscriber-side gating
	// works whether the caller supplies projectID or not.
	if payload == nil {
		payload = map[string]any{}
	}
	if projectID != "" {
		if _, ok := payload["project_id"]; !ok {
			payload["project_id"] = projectID
		}
	}
	ev := wshandler.WireEvent{
		ID:          s.nextID(),
		Topic:       wshandler.TopicPrefix + workspaceID,
		Kind:        kind,
		WorkspaceID: workspaceID,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Data:        payload,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, ok := s.byTopic[ev.Topic]
	if !ok {
		return
	}
	for _, entry := range subs {
		if entry.projectID != "" {
			pid, _ := ev.Data["project_id"].(string)
			if pid != entry.projectID {
				continue
			}
		}
		select {
		case entry.ch <- ev:
		default:
			// Channel full — drop. Parity with
			// SurrealLiveSource.fanout's drop-on-full semantics.
		}
	}
}

func (s *harnessSource) requireMember(ctx context.Context, workspaceID, userID string) error {
	if s.members == nil {
		return wshandler.ErrNotMember
	}
	ok, err := s.members.IsMember(ctx, workspaceID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return wshandler.ErrNotMember
	}
	return nil
}

// nextID returns a string-comparable monotonic id mirroring the
// production wireIDCounter shape (20-digit zero-padded) so test
// clients that compare ids lexicographically see the same ordering
// invariant.
func (s *harnessSource) nextID() string {
	n := s.idCounter.Add(1)
	raw := strconv.FormatUint(n, 10)
	if len(raw) >= 20 {
		return raw
	}
	pad := make([]byte, 20-len(raw))
	for i := range pad {
		pad[i] = '0'
	}
	return string(pad) + raw
}
