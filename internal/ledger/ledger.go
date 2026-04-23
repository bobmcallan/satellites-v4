// Package ledger is the satellites-v4 append-only event log primitive.
// Every durable decision/evidence/trace in a project lands here as a row;
// later primitives (stories, tasks, repo scans) emit rows as their audit
// chain. Append-only at the Store interface level — no Update or Delete
// verbs exist.
package ledger

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// LedgerEntry is a single append-only row. No UpdatedAt field — once
// written, entries do not mutate. WorkspaceID cascades from the parent
// project at write time per docs/architecture.md §8.
type LedgerEntry struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	ProjectID   string    `json:"project_id"`
	Type        string    `json:"type"`
	Content     string    `json:"content"`
	Actor       string    `json:"actor"`
	CreatedAt   time.Time `json:"created_at"`
}

// NewID returns a fresh entry id in the canonical `ldg_<8hex>` form.
func NewID() string {
	return fmt.Sprintf("ldg_%s", uuid.NewString()[:8])
}
