// Package busparity is the dual-emit parity verifier for
// epic:surreal-live-migration order:03 (sty_2ba48616). The verifier
// subscribes to both the in-process hub and the surreallive
// Subscriber, correlates events by (table, row_id) inside a sliding
// window, writes kind:bus-parity-mismatch ledger rows when one bus
// sees an event the other doesn't, and writes kind:bus-parity-stats
// rows periodically so the operator has running counts.
//
// The verifier ships under SATELLITES_BUS_PARITY_VERIFIER=true. The
// 7-day pprod observation that gates order:04 is calendar work the
// operator owns; this package emits the data the gate consumes.
//
// Scope notes (per docs/surreal-live-feasibility.md):
//
//   - Correlation key is (table, row_id). The hub emits Kind shapes
//     like "task.<status>" while LIVE emits "<table>.<action>";
//     correlating on row id sidesteps the kind-shape mismatch.
//   - Payload-diff detection is out of scope for v1. A row id seen
//     on one bus and not the other within the window emits a
//     hub_only/live_only mismatch; payload bytes are not compared.
//   - The window is bounded by ExpiryWindow (default 60s). Entries
//     older than that without a matched peer are recorded as a
//     mismatch on the next sweep.
package busparity

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// DefaultExpiryWindow caps how long a single-bus observation waits for
// its peer before being classified as a mismatch.
const DefaultExpiryWindow = 60 * time.Second

// DefaultStatsInterval governs the cadence of `kind:bus-parity-stats`
// emissions.
const DefaultStatsInterval = 60 * time.Second

// Source identifies which bus surfaced an observation.
type Source string

const (
	SourceHub  Source = "hub"
	SourceLive Source = "live"
)

// Observation is the per-event input the verifier correlates. Callers
// (the boot wiring in cmd/satellites/main.go) translate native bus
// events into this shape.
type Observation struct {
	Source      Source
	Table       string // "tasks" | "stories" | "ledger"
	RowID       string // task_id / story_id / ledger row id
	WorkspaceID string
	ProjectID   string
	SeenAt      time.Time
}

// Config parameterises a Verifier. Zero-valued fields fall to documented
// defaults.
type Config struct {
	ExpiryWindow  time.Duration
	StatsInterval time.Duration
	ProjectID     string // ledger rows are project-scoped
}

// Verifier holds the correlation state. Methods are concurrency-safe.
type Verifier struct {
	cfg    Config
	ledger ledger.Store
	logger arbor.ILogger

	mu      sync.Mutex
	pending map[string]Observation // keyed by table+rowID
	counts  Counts
}

// Counts is the rolling tally between two stats emissions; the
// verifier resets it after each emit.
type Counts struct {
	Matched  int `json:"matched"`
	HubOnly  int `json:"hub_only"`
	LiveOnly int `json:"live_only"`
}

// New returns a Verifier. ledgerStore must be non-nil; logger may be
// nil (drops log lines).
func New(ledgerStore ledger.Store, cfg Config, logger arbor.ILogger) *Verifier {
	if cfg.ExpiryWindow <= 0 {
		cfg.ExpiryWindow = DefaultExpiryWindow
	}
	if cfg.StatsInterval <= 0 {
		cfg.StatsInterval = DefaultStatsInterval
	}
	return &Verifier{
		cfg:     cfg,
		ledger:  ledgerStore,
		logger:  logger,
		pending: make(map[string]Observation),
	}
}

// Observe records a single bus observation. If the peer observation
// already lives in the pending map, both entries are removed and
// matched counts incremented; otherwise the observation is added to
// pending.
func (v *Verifier) Observe(o Observation) {
	if v == nil || o.Table == "" || o.RowID == "" {
		return
	}
	if o.SeenAt.IsZero() {
		o.SeenAt = time.Now().UTC()
	}
	key := o.Table + ":" + o.RowID
	v.mu.Lock()
	defer v.mu.Unlock()
	existing, ok := v.pending[key]
	if !ok {
		v.pending[key] = o
		return
	}
	if existing.Source == o.Source {
		// Same bus reasserting the same row inside the window —
		// keep the latest, no match.
		v.pending[key] = o
		return
	}
	// Peer found on the other bus → matched.
	delete(v.pending, key)
	v.counts.Matched++
}

// Sweep is the periodic tick the Run loop calls. It expires
// pending observations older than ExpiryWindow as mismatches and
// (when interval has elapsed) emits a stats row.
func (v *Verifier) Sweep(ctx context.Context, now time.Time) {
	if v == nil {
		return
	}
	expired := v.pruneExpired(now)
	for _, ob := range expired {
		v.emitMismatch(ctx, ob, now)
	}
}

// EmitStats writes a `kind:bus-parity-stats` ledger row carrying the
// rolling counts and resets them. Callers (Run) drive the cadence.
func (v *Verifier) EmitStats(ctx context.Context, now time.Time) {
	if v == nil || v.ledger == nil {
		return
	}
	v.mu.Lock()
	c := v.counts
	v.counts = Counts{}
	pendingHub := 0
	pendingLive := 0
	for _, ob := range v.pending {
		if ob.Source == SourceHub {
			pendingHub++
		} else {
			pendingLive++
		}
	}
	v.mu.Unlock()
	payload := map[string]any{
		"matched":      c.Matched,
		"hub_only":     c.HubOnly,
		"live_only":    c.LiveOnly,
		"pending_hub":  pendingHub,
		"pending_live": pendingLive,
		"window_s":     int(v.cfg.ExpiryWindow.Seconds()),
		"interval_s":   int(v.cfg.StatsInterval.Seconds()),
	}
	body, _ := json.Marshal(payload)
	_, err := v.ledger.Append(ctx, ledger.LedgerEntry{
		ProjectID:  v.cfg.ProjectID,
		Type:       ledger.TypeEvidence,
		Tags:       []string{"kind:bus-parity-stats"},
		Content:    fmt.Sprintf("bus parity stats: matched=%d hub_only=%d live_only=%d", c.Matched, c.HubOnly, c.LiveOnly),
		Structured: body,
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceSystem,
		Status:     ledger.StatusActive,
		CreatedBy:  "system:bus-parity-verifier",
	}, now)
	if err != nil && v.logger != nil {
		v.logger.Warn().Str("error", err.Error()).Msg("busparity: stats append failed")
	}
}

// Run drives the sweep + stats cadence until ctx is cancelled. Callers
// fan their bus events into Observe from independent goroutines.
func (v *Verifier) Run(ctx context.Context) {
	if v == nil {
		return
	}
	stats := time.NewTicker(v.cfg.StatsInterval)
	sweep := time.NewTicker(v.cfg.ExpiryWindow / 2)
	defer stats.Stop()
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-sweep.C:
			v.Sweep(ctx, now.UTC())
		case now := <-stats.C:
			v.EmitStats(ctx, now.UTC())
		}
	}
}

// pruneExpired removes entries older than ExpiryWindow and bumps the
// hub_only / live_only counts so EmitStats reflects the period's
// totals. Returns the removed observations so the caller can write
// per-mismatch ledger rows.
func (v *Verifier) pruneExpired(now time.Time) []Observation {
	v.mu.Lock()
	defer v.mu.Unlock()
	cutoff := now.Add(-v.cfg.ExpiryWindow)
	var expired []Observation
	for k, ob := range v.pending {
		if ob.SeenAt.Before(cutoff) {
			expired = append(expired, ob)
			if ob.Source == SourceHub {
				v.counts.HubOnly++
			} else {
				v.counts.LiveOnly++
			}
			delete(v.pending, k)
		}
	}
	return expired
}

// emitMismatch writes a kind:bus-parity-mismatch ledger row for one
// expired observation. Mismatch classification matches the source bus:
// hub_only when the hub observation timed out without a live peer,
// live_only when LIVE timed out without a hub peer.
func (v *Verifier) emitMismatch(ctx context.Context, ob Observation, now time.Time) {
	if v.ledger == nil {
		return
	}
	classification := "hub_only"
	if ob.Source == SourceLive {
		classification = "live_only"
	}
	payload := map[string]any{
		"classification": classification,
		"source":         string(ob.Source),
		"table":          ob.Table,
		"row_id":         ob.RowID,
		"workspace_id":   ob.WorkspaceID,
		"project_id":     ob.ProjectID,
		"seen_at":        ob.SeenAt.UTC().Format(time.RFC3339Nano),
	}
	body, _ := json.Marshal(payload)
	_, err := v.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: ob.WorkspaceID,
		ProjectID:   v.cfg.ProjectID,
		Type:        ledger.TypeEvidence,
		Tags: []string{
			"kind:bus-parity-mismatch",
			"classification:" + classification,
			"table:" + ob.Table,
		},
		Content:    fmt.Sprintf("bus parity mismatch: %s table=%s row_id=%s", classification, ob.Table, ob.RowID),
		Structured: body,
		Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceSystem,
		Status:     ledger.StatusActive,
		CreatedBy:  "system:bus-parity-verifier",
	}, now)
	if err != nil && v.logger != nil {
		v.logger.Warn().Str("error", err.Error()).Msg("busparity: mismatch append failed")
	}
}
