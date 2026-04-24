package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStore_RegisterThenGet(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	sess, err := s.Register(context.Background(), "u1", "abc", SourceSessionStart, now)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if sess.UserID != "u1" || sess.SessionID != "abc" {
		t.Fatalf("shape: %+v", sess)
	}
	got, err := s.Get(context.Background(), "u1", "abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.LastSeenAt.Equal(now) {
		t.Fatalf("last_seen_at: got %v want %v", got.LastSeenAt, now)
	}
}

func TestMemoryStore_Get_NotFound(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	_, err := s.Get(context.Background(), "u1", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_Touch(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	t0 := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	if _, err := s.Register(context.Background(), "u1", "abc", SourceSessionStart, t0); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := s.Touch(context.Background(), "u1", "abc", t1)
	if err != nil {
		t.Fatalf("touch: %v", err)
	}
	if !got.LastSeenAt.Equal(t1) {
		t.Fatalf("last_seen_at not updated: got %v want %v", got.LastSeenAt, t1)
	}
	if !got.Registered.Equal(t0) {
		t.Fatalf("registered_at clobbered: got %v want %v", got.Registered, t0)
	}
}

func TestIsStale(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		lastSeen  time.Time
		now       time.Time
		staleness time.Duration
		want      bool
	}{
		{"fresh", base, base.Add(5 * time.Minute), StalenessDefault, false},
		{"edge_not_stale", base, base.Add(StalenessDefault), StalenessDefault, false},
		{"stale", base, base.Add(StalenessDefault + time.Second), StalenessDefault, true},
		{"explicit_staleness_stale", base, base.Add(2 * time.Minute), time.Minute, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := IsStale(Session{LastSeenAt: c.lastSeen}, c.now, c.staleness)
			if got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestMemoryStore_SetOrchestratorGrant(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()
	t0 := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	if _, err := s.Register(ctx, "u1", "abc", SourceSessionStart, t0); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := s.SetOrchestratorGrant(ctx, "u1", "abc", "grant_xyz", t1)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got.OrchestratorGrantID != "grant_xyz" {
		t.Fatalf("grant id not stamped: %q", got.OrchestratorGrantID)
	}
	if !got.LastSeenAt.Equal(t1) {
		t.Fatalf("last_seen_at not touched: got %v want %v", got.LastSeenAt, t1)
	}
	read, err := s.Get(ctx, "u1", "abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.OrchestratorGrantID != "grant_xyz" {
		t.Fatalf("grant id not persisted: %q", read.OrchestratorGrantID)
	}
}

func TestMemoryStore_SetOrchestratorGrant_NotFound(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	_, err := s.SetOrchestratorGrant(context.Background(), "u1", "missing", "grant_xyz", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_RegisterIdempotent(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	t0 := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	first, _ := s.Register(context.Background(), "u1", "abc", SourceSessionStart, t0)
	if !first.Registered.Equal(t0) {
		t.Fatalf("first registered_at: %v", first.Registered)
	}
	second, _ := s.Register(context.Background(), "u1", "abc", SourceEnforceHook, t1)
	if !second.Registered.Equal(t0) {
		t.Fatalf("registered_at changed on re-register: %v", second.Registered)
	}
	if !second.LastSeenAt.Equal(t1) {
		t.Fatalf("last_seen_at: got %v want %v", second.LastSeenAt, t1)
	}
	if second.Source != SourceEnforceHook {
		t.Fatalf("source: got %q want %q", second.Source, SourceEnforceHook)
	}
}
