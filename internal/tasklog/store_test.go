package tasklog

import (
	"context"
	"sync"
	"testing"
	"time"
)

const (
	testTaskID = "task_logtest"
	testWsID   = "wksp_logtest"
)

func newEntry(seq int64, kind string) Entry {
	return Entry{
		TaskID:      testTaskID,
		WorkspaceID: testWsID,
		Seq:         seq,
		Kind:        kind,
	}
}

func TestMemoryStore_AppendAndListInOrder(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	for i := int64(0); i < 5; i++ {
		kind := KindStdout
		if i == 0 {
			kind = KindStart
		} else if i == 4 {
			kind = KindStop
		}
		if _, err := s.Append(ctx, newEntry(i, kind), now.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatalf("Append seq=%d: %v", i, err)
		}
	}

	rows, err := s.List(ctx, ListOptions{TaskID: testTaskID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("List returned %d rows, want 5", len(rows))
	}
	for i, r := range rows {
		if r.Seq != int64(i) {
			t.Errorf("rows[%d].Seq = %d, want %d", i, r.Seq, i)
		}
	}
	if rows[0].Kind != KindStart || rows[4].Kind != KindStop {
		t.Errorf("frame boundaries wrong: first=%s last=%s", rows[0].Kind, rows[4].Kind)
	}
}

func TestMemoryStore_ListFromSeqInclusive(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	for i := int64(0); i < 6; i++ {
		_, _ = s.Append(ctx, newEntry(i, KindStdout), now)
	}
	rows, err := s.List(ctx, ListOptions{TaskID: testTaskID, FromSeq: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	if rows[0].Seq != 3 {
		t.Errorf("first.Seq = %d, want 3", rows[0].Seq)
	}
}

func TestMemoryStore_ListLimit(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	for i := int64(0); i < 10; i++ {
		_, _ = s.Append(ctx, newEntry(i, KindStdout), now)
	}
	rows, err := s.List(ctx, ListOptions{TaskID: testTaskID, Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
}

func TestMemoryStore_SubscribeReceivesLiveRows(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()

	// Seed one row so wsByID is populated for membership scoping.
	if _, err := s.Append(ctx, newEntry(0, KindStart), now); err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	ch, cancel, err := s.Subscribe(ctx, testTaskID, []string{testWsID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	received := make([]int64, 0, 5)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			select {
			case e := <-ch:
				received = append(received, e.Seq)
			case <-time.After(time.Second):
				t.Errorf("subscribe receive %d timeout", i)
				return
			}
		}
	}()
	for i := int64(1); i < 6; i++ {
		_, _ = s.Append(ctx, newEntry(i, KindStdout), now)
	}
	wg.Wait()

	for i, seq := range received {
		if seq != int64(i+1) {
			t.Errorf("received[%d] = %d, want %d", i, seq, i+1)
		}
	}
}

func TestMemoryStore_SubscribeCancelStopsDelivery(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	_, _ = s.Append(ctx, newEntry(0, KindStart), now)

	_, cancel, err := s.Subscribe(ctx, testTaskID, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.subs[testTaskID] {
		if !sub.cancelled {
			t.Errorf("post-cancel sub not flagged cancelled")
		}
	}
	if len(s.subs[testTaskID]) != 0 {
		t.Errorf("post-cancel sweep left %d slots", len(s.subs[testTaskID]))
	}
}

func TestMemoryStore_AppendRejectsInvalidKind(t *testing.T) {
	s := NewMemoryStore()
	e := newEntry(0, "bogus")
	if _, err := s.Append(context.Background(), e, time.Now().UTC()); err == nil {
		t.Fatalf("Append accepted invalid kind")
	}
}

func TestMemoryStore_AppendRequiresTaskID(t *testing.T) {
	s := NewMemoryStore()
	e := Entry{WorkspaceID: testWsID, Seq: 0, Kind: KindStart}
	if _, err := s.Append(context.Background(), e, time.Now().UTC()); err == nil {
		t.Fatalf("Append accepted empty task_id")
	}
}

func TestMemoryStore_ListMembershipScoping(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now().UTC()
	_, _ = s.Append(context.Background(), newEntry(0, KindStart), now)

	rows, err := s.List(context.Background(), ListOptions{
		TaskID:      testTaskID,
		Memberships: []string{"wksp_other"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("scoped List returned %d rows, want 0", len(rows))
	}

	rows, err = s.List(context.Background(), ListOptions{
		TaskID:      testTaskID,
		Memberships: []string{testWsID},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("matching List returned %d rows, want 1", len(rows))
	}
}

func TestMemoryStore_ConcurrentAppendPreservesAllRows(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	var wg sync.WaitGroup
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(seq int64) {
			defer wg.Done()
			_, _ = s.Append(ctx, newEntry(seq, KindStdout), now)
		}(int64(i))
	}
	wg.Wait()
	rows, err := s.List(ctx, ListOptions{TaskID: testTaskID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != N {
		t.Fatalf("List returned %d rows, want %d", len(rows), N)
	}
}
