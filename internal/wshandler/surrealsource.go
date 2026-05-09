// SurrealLiveSource is the production EventSource — a fan-out adapter
// over internal/surreallive that translates incoming notifications
// into WireEvents and dispatches them to per-subscriber channels keyed
// by topic. Workspace membership is enforced at Subscribe time.

package wshandler

import (
	"context"
	"sync"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/surreallive"
)

// SurrealLiveSource implements EventSource. Construct with
// NewSurrealLiveSource passing the live subscriber, the membership
// store, and the logger; then call Run from a goroutine to start the
// per-table dial loops.
type SurrealLiveSource struct {
	live    *surreallive.Subscriber
	members Membership
	logger  arbor.ILogger

	mu        sync.Mutex
	subs      map[string]*subEntry          // subID → entry
	byTopic   map[string]map[string]*subEntry // topic → subID → entry
}

type subEntry struct {
	topic       string
	workspaceID string
	userID      string
	ch          chan WireEvent
}

// chanBuf matches the legacy hub's per-subscriber buffer.
const chanBuf = 64

// NewSurrealLiveSource constructs a SurrealLiveSource. live + members
// must be non-nil; logger may be nil (drops log lines).
func NewSurrealLiveSource(live *surreallive.Subscriber, members Membership, logger arbor.ILogger) *SurrealLiveSource {
	return &SurrealLiveSource{
		live:    live,
		members: members,
		logger:  logger,
		subs:    make(map[string]*subEntry),
		byTopic: make(map[string]map[string]*subEntry),
	}
}

// Run kicks off the per-table dial loops. Returns when ctx is
// cancelled or every dial loop has exited. Subscribers' channels
// stay open until the parent context cancels.
func (s *SurrealLiveSource) Run(ctx context.Context) {
	if s == nil || s.live == nil {
		return
	}
	tables := []string{"tasks", "stories", "ledger"}
	var wg sync.WaitGroup
	for _, table := range tables {
		wg.Add(1)
		go func(table string) {
			defer wg.Done()
			err := s.live.Subscribe(ctx, table, nil, func(ev surreallive.Event) {
				s.fanout(translate(ev))
			})
			if err != nil && err != context.Canceled && s.logger != nil {
				s.logger.Warn().Str("table", table).Str("error", err.Error()).
					Msg("wshandler/surrealsource: subscribe ended")
			}
		}(table)
	}
	wg.Wait()
}

// Subscribe implements EventSource. Validates membership, allocates a
// buffered channel, registers it under (topic, subscriberID).
// Re-registering an existing subscriberID drops the prior channel.
func (s *SurrealLiveSource) Subscribe(ctx context.Context, topic, subscriberID, userID string) (<-chan WireEvent, error) {
	wsID, err := ParseTopicWorkspace(topic)
	if err != nil {
		return nil, err
	}
	if err := s.requireMember(ctx, wsID, userID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribeLocked(subscriberID)
	ch := make(chan WireEvent, chanBuf)
	entry := &subEntry{
		topic:       topic,
		workspaceID: wsID,
		userID:      userID,
		ch:          ch,
	}
	s.subs[subscriberID] = entry
	if s.byTopic[topic] == nil {
		s.byTopic[topic] = make(map[string]*subEntry)
	}
	s.byTopic[topic][subscriberID] = entry
	return ch, nil
}

// Unsubscribe implements EventSource.
func (s *SurrealLiveSource) Unsubscribe(subscriberID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribeLocked(subscriberID)
}

func (s *SurrealLiveSource) unsubscribeLocked(subscriberID string) {
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

// fanout dispatches each WireEvent to every subscriber of its topic.
// Drops the event for any subscriber whose channel is full — the
// legacy hub used a two-strike eviction; the surreallive cutover
// keeps the simpler drop-on-full semantics. Operators see this in
// agent or portal "lagging" behaviour rather than disconnects;
// subscribers are still receiving subsequent events.
func (s *SurrealLiveSource) fanout(events []WireEvent) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range events {
		subs, ok := s.byTopic[ev.Topic]
		if !ok {
			continue
		}
		for _, entry := range subs {
			select {
			case entry.ch <- ev:
			default:
				// Channel full — drop. See doc comment.
			}
		}
	}
}

func (s *SurrealLiveSource) requireMember(ctx context.Context, workspaceID, userID string) error {
	if s.members == nil {
		// No membership store wired — refuse rather than allow.
		return ErrNotMember
	}
	ok, err := s.members.IsMember(ctx, workspaceID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	return nil
}
