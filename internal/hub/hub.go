// Package hub is the in-process fan-out primitive for workspace-scoped
// live-state events (story_fe07e6bb, docs/architecture.md §6).
//
// Subscribers receive events published to their topic string. Topics are
// opaque to the hub; callers convention them as "ws:<workspace_id>" so
// tenancy gating happens at the caller, not here. Replay is a per-topic
// ring buffer capped at 500. Back-pressure: each subscriber channel is
// buffered 64, marked lagging on first overflow, evicted on the second.
package hub

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultReplayCap = 500
	defaultChanBuf   = 64
	evictedKind      = "hub.evicted"
)

// Event is the unit fanned out to subscribers. ID is assigned by the hub
// at Publish time and is monotonic across the hub (not per-topic), so
// string comparison matches insertion order. WorkspaceID is populated by
// publishers that originate a workspace-scoped mutation; the AuthHub
// wrapper (slice 10.2) validates it against the topic's workspace suffix.
// CreatedAt is stamped by the hub at Publish time when the caller leaves
// it zero (slice 10.3). The format is RFC3339 when serialised via JSON.
type Event struct {
	ID          string
	Topic       string
	Kind        string
	WorkspaceID string
	CreatedAt   time.Time
	Data        any
}

type subscription struct {
	id     string
	topic  string
	ch     chan Event
	strike int
	dead   bool
}

type ring struct {
	events []Event
	head   int
	size   int
	cap    int
}

func newRing(capacity int) *ring {
	return &ring{events: make([]Event, capacity), cap: capacity}
}

func (r *ring) push(e Event) {
	r.events[r.head] = e
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}

// since returns events in insertion order whose ID is lexicographically
// greater than sinceID. Empty sinceID returns the full buffer contents.
func (r *ring) since(sinceID string) []Event {
	if r.size == 0 {
		return nil
	}
	out := make([]Event, 0, r.size)
	start := (r.head - r.size + r.cap) % r.cap
	for i := 0; i < r.size; i++ {
		e := r.events[(start+i)%r.cap]
		if sinceID == "" || e.ID > sinceID {
			out = append(out, e)
		}
	}
	return out
}

// Hub fans events out to per-topic subscribers with ring-buffer replay
// and per-subscriber back-pressure. Safe for concurrent Subscribe,
// Unsubscribe, Publish, and ReplayBuffer calls.
type Hub struct {
	mu        sync.Mutex
	topics    map[string]map[string]*subscription
	rings     map[string]*ring
	idCounter atomic.Uint64
	replayCap int
	chanBuf   int
}

// New returns a Hub with production defaults: replay cap 500, per-subscriber
// channel buffer 64.
func New() *Hub {
	return &Hub{
		topics:    make(map[string]map[string]*subscription),
		rings:     make(map[string]*ring),
		replayCap: defaultReplayCap,
		chanBuf:   defaultChanBuf,
	}
}

// Subscribe registers subscriberID against topic and returns a receive
// channel. If subscriberID is already registered (on any topic), the prior
// registration is dropped first — duplicate IDs are not allowed.
func (h *Hub) Subscribe(topic, subscriberID string) <-chan Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unsubscribeLocked(subscriberID)
	return h.subscribeLocked(topic, subscriberID)
}

// SubscribeSince is Subscribe plus an initial replay of events whose ID is
// greater than sinceID. Replayed events are enqueued onto the returned
// channel before it is returned; if replay exceeds the channel buffer, the
// oldest replay entries are dropped (caller is expected to drain promptly).
func (h *Hub) SubscribeSince(topic, subscriberID, sinceID string) <-chan Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unsubscribeLocked(subscriberID)
	ch := h.subscribeLocked(topic, subscriberID)
	if r, ok := h.rings[topic]; ok {
		for _, e := range r.since(sinceID) {
			select {
			case ch <- e:
			default:
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- e:
				default:
				}
			}
		}
	}
	return ch
}

func (h *Hub) subscribeLocked(topic, subscriberID string) chan Event {
	ch := make(chan Event, h.chanBuf)
	sub := &subscription{id: subscriberID, topic: topic, ch: ch}
	subs, ok := h.topics[topic]
	if !ok {
		subs = make(map[string]*subscription)
		h.topics[topic] = subs
	}
	subs[subscriberID] = sub
	return ch
}

// Unsubscribe removes subscriberID from its topic and closes its channel.
// No-op if the ID is unknown.
func (h *Hub) Unsubscribe(subscriberID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unsubscribeLocked(subscriberID)
}

func (h *Hub) unsubscribeLocked(subscriberID string) {
	for topic, subs := range h.topics {
		if sub, ok := subs[subscriberID]; ok {
			if !sub.dead {
				close(sub.ch)
				sub.dead = true
			}
			delete(subs, subscriberID)
			if len(subs) == 0 {
				delete(h.topics, topic)
			}
			return
		}
	}
}

// Publish stamps event.ID (overwriting any caller-supplied value), appends
// it to the topic's ring buffer, and fans it out to subscribers of that
// topic. A subscriber whose channel is full is marked lagging on the first
// overflow and evicted on the second; eviction closes the subscriber's
// channel and emits a follow-up evictedKind event to the same topic.
func (h *Hub) Publish(topic string, event Event) {
	h.mu.Lock()
	evicted := h.publishLocked(topic, event)
	h.mu.Unlock()

	for _, id := range evicted {
		h.Publish(topic, Event{Kind: evictedKind, Data: id})
	}
}

func (h *Hub) publishLocked(topic string, event Event) []string {
	event.ID = h.nextID()
	event.Topic = topic
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}

	r, ok := h.rings[topic]
	if !ok {
		r = newRing(h.replayCap)
		h.rings[topic] = r
	}
	r.push(event)

	subs := h.topics[topic]
	if len(subs) == 0 {
		return nil
	}
	var evicted []string
	for _, sub := range subs {
		if sub.dead {
			continue
		}
		select {
		case sub.ch <- event:
			sub.strike = 0
		default:
			sub.strike++
			if sub.strike >= 2 {
				sub.dead = true
				close(sub.ch)
				evicted = append(evicted, sub.id)
			}
		}
	}
	for _, id := range evicted {
		delete(subs, id)
	}
	if len(subs) == 0 {
		delete(h.topics, topic)
	}
	return evicted
}

// ReplayBuffer returns buffered events for topic whose ID is greater than
// sinceID, in insertion order. Empty sinceID returns the full buffer (up to
// the cap). Unknown topic returns nil.
func (h *Hub) ReplayBuffer(topic, sinceID string) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rings[topic]
	if !ok {
		return nil
	}
	return r.since(sinceID)
}

func (h *Hub) nextID() string {
	return fmt.Sprintf("%020d", h.idCounter.Add(1))
}
