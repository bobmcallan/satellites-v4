// Package story is the satellites-v4 story primitive — the unit of
// deliverable work per principle pr_a9ccecfb ("story is the unit of work;
// epics are tags, not primitives"). Status transitions emit ledger rows
// so the audit chain is intact from the first write per pr_20440c77.
package story

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Story is the unit of deliverable work. Tags carry epic membership via
// the `epic:<slug>` convention. WorkspaceID cascades from the parent
// project at write time per docs/architecture.md §8.
type Story struct {
	ID                 string    `json:"id"`
	WorkspaceID        string    `json:"workspace_id"`
	ProjectID          string    `json:"project_id"`
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	AcceptanceCriteria string    `json:"acceptance_criteria"`
	Status             string    `json:"status"`
	Priority           string    `json:"priority"`
	Category           string    `json:"category"`
	Tags               []string  `json:"tags"`
	CreatedBy          string    `json:"created_by"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// NewID returns a fresh story id in the canonical `sty_<8hex>` form.
func NewID() string {
	return fmt.Sprintf("sty_%s", uuid.NewString()[:8])
}
