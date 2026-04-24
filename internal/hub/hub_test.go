package hub

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTopic = "ws:wksp_A"

// TestPubSubFanout_10Subscribers covers AC1 + AC2: a single Publish on a
// topic reaches every subscriber on that topic.
func TestPubSubFanout_10Subscribers(t *testing.T) {
	h := New()
	const n = 10
	chs := make([]<-chan Event, n)
	for i := 0; i < n; i++ {
		chs[i] = h.Subscribe(testTopic, fmt.Sprintf("sub-%d", i))
	}

	h.Publish(testTopic, Event{Kind: "ping", Data: "hello"})

	for i, ch := range chs {
		select {
		case ev := <-ch:
			assert.Equal(t, "ping", ev.Kind, "sub %d kind", i)
			assert.Equal(t, testTopic, ev.Topic, "sub %d topic", i)
			assert.NotEmpty(t, ev.ID, "sub %d id stamped", i)
		case <-time.After(time.Second):
			t.Fatalf("sub %d did not receive event", i)
		}
	}
}

// TestReplayBufferSinceID covers AC3: ReplayBuffer returns only events
// whose ID is greater than the supplied sinceID, in insertion order.
func TestReplayBufferSinceID(t *testing.T) {
	h := New()
	// No subscribers — events go to the ring buffer only.
	for i := 0; i < 5; i++ {
		h.Publish(testTopic, Event{Kind: "k", Data: i})
	}
	all := h.ReplayBuffer(testTopic, "")
	require.Len(t, all, 5)

	since := h.ReplayBuffer(testTopic, all[1].ID) // strictly after event #2
	require.Len(t, since, 3)
	assert.Equal(t, all[2].ID, since[0].ID)
	assert.Equal(t, all[3].ID, since[1].ID)
	assert.Equal(t, all[4].ID, since[2].ID)
}

// TestReplayCap500 covers AC3 + AC6: the ring buffer is capped at 500 and
// publishing beyond the cap drops the oldest entries without unbounded
// memory growth.
func TestReplayCap500(t *testing.T) {
	h := New()
	const total = 600
	for i := 0; i < total; i++ {
		h.Publish(testTopic, Event{Kind: "k", Data: i})
	}
	buf := h.ReplayBuffer(testTopic, "")
	require.Len(t, buf, defaultReplayCap)

	// Oldest retained event should carry Data = total-cap = 100
	// (events 0..99 were dropped as the ring wrapped).
	assert.Equal(t, 100, buf[0].Data)
	assert.Equal(t, total-1, buf[len(buf)-1].Data)

	// Cap applies across further publishes — still 500, not growing.
	h.Publish(testTopic, Event{Kind: "extra"})
	assert.Len(t, h.ReplayBuffer(testTopic, ""), defaultReplayCap)
}

// TestBackpressureEviction covers AC4: a subscriber that does not drain
// its channel is marked lagging on the first overflow and evicted on the
// second, producing a hub.evicted follow-up event observable by a healthy
// peer subscriber on the same topic.
func TestBackpressureEviction(t *testing.T) {
	h := New()
	laggard := h.Subscribe(testTopic, "laggard")

	// Fill the laggard channel to capacity (64). Healthy is subscribed
	// afterwards so its channel is empty when the overflow publishes
	// arrive — otherwise the rapid fill burst can race the drain loop
	// and trip healthy's own lagging strike, which makes the test flaky.
	for i := 0; i < defaultChanBuf; i++ {
		h.Publish(testTopic, Event{Kind: "fill", Data: i})
	}

	healthy := h.Subscribe(testTopic, "healthy")

	var healthyEvents []Event
	var hmu sync.Mutex
	done := make(chan struct{})
	go func() {
		for ev := range healthy {
			hmu.Lock()
			healthyEvents = append(healthyEvents, ev)
			hmu.Unlock()
		}
		close(done)
	}()

	// 65th publish overflows laggard: strike=1, lagging, NOT yet evicted.
	h.Publish(testTopic, Event{Kind: "overflow-1"})
	assert.False(t, laggardDead(h), "not evicted after first overflow")

	// 66th publish overflows again: strike=2, evict + hub.evicted event.
	h.Publish(testTopic, Event{Kind: "overflow-2"})

	// Laggard channel should be closed (evicted).
	require.Eventually(t, func() bool {
		return laggardDead(h)
	}, time.Second, 5*time.Millisecond, "laggard evicted after second overflow")

	// Drain whatever laggard had buffered + confirm it's closed.
	drained := 0
	for range laggard {
		drained++
	}
	assert.Equal(t, defaultChanBuf, drained, "laggard yielded its 64 buffered events before close")

	// Healthy sub should have received the hub.evicted notice for the laggard.
	h.Unsubscribe("healthy")
	<-done

	hmu.Lock()
	defer hmu.Unlock()
	var sawEvicted bool
	for _, ev := range healthyEvents {
		if ev.Kind == evictedKind {
			assert.Equal(t, "laggard", ev.Data, "hub.evicted carries the evicted sub id")
			sawEvicted = true
			break
		}
	}
	assert.True(t, sawEvicted, "hub.evicted event fanned out to healthy peers")
}

// laggardDead probes the hub for eviction state via ReplayBuffer as a side
// channel — the hub does not expose internal sub state, so instead we
// check that a fresh publish does not deadlock and that the laggard
// channel is closed. We infer closedness by topic membership: after
// eviction, the internal topics map drops the entry if no subs remain,
// but here healthy is still present. Use a helper that reaches into the
// hub's mutex-guarded state for test observability.
func laggardDead(h *Hub) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs, ok := h.topics[testTopic]
	if !ok {
		return true
	}
	_, present := subs["laggard"]
	return !present
}

// TestConcurrentSubscribeUnsubscribe covers AC5: hub remains correct under
// mixed concurrent Publish + Subscribe + Unsubscribe load, with no data
// races (when run with -race) and no lost events for a stable reference
// subscriber.
func TestConcurrentSubscribeUnsubscribe(t *testing.T) {
	h := New()
	stableCh := h.Subscribe(testTopic, "stable")

	const publishes = 500
	const workers = 8

	var received atomic.Int64
	drainDone := make(chan struct{})
	go func() {
		for range stableCh {
			received.Add(1)
		}
		close(drainDone)
	}()

	// Churn subscribers.
	var wg sync.WaitGroup
	churnDone := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id := "churn-" + strconv.Itoa(w) + "-" + strconv.Itoa(i)
				ch := h.Subscribe(testTopic, id)
				// Drain briefly to avoid eviction spam interfering with
				// stable-sub accounting.
				drained := 0
				for drained < 2 {
					select {
					case <-ch:
						drained++
					case <-churnDone:
						h.Unsubscribe(id)
						return
					case <-time.After(2 * time.Millisecond):
						drained = 2
					}
				}
				h.Unsubscribe(id)
			}
		}(w)
	}

	// Publisher: pace publishes so the stable sub (buf 64) can keep up.
	for i := 0; i < publishes; i++ {
		h.Publish(testTopic, Event{Kind: "pulse", Data: i})
		if i%32 == 31 {
			// Give the drain goroutine a chance to catch up.
			time.Sleep(time.Millisecond)
		}
	}
	close(churnDone)
	wg.Wait()

	// Stable sub must still be present — evict it explicitly to close ch.
	h.Unsubscribe("stable")
	<-drainDone

	assert.Equal(t, int64(publishes), received.Load(),
		"stable subscriber received every event without loss")
}
