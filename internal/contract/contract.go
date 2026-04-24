// Package contract is the satellites-v4 contract_instance primitive: the
// ordered list of CI rows for a story IS the workflow per docs/
// architecture.md §5 ("Workflow is a list of contract names per story").
// The contract definition itself lives as a `document{type=contract}` row
// from story_509f1111; this package only adds the CI table + FK.
//
// No Delete verb: CIs persist for audit per principle pr_0c11b762
// ("Evidence is the primary trust leverage").
package contract

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ContractInstance is one slot in a story's workflow. Fields match
// docs/architecture.md §5 verbatim. WorkspaceID + ProjectID cascade from
// the parent story at Create time per principle pr_0779e5af.
//
// ClaimedViaGrantID (story_85675c33) is populated when the caller's
// session has an orchestrator grant and the contract specifies a
// required_role. It coexists with ClaimedBySessionID during the
// transitional period; the session_id field will be dropped in a
// cleanup follow-up once every write path references the grant id.
type ContractInstance struct {
	ID                 string    `json:"id"`
	WorkspaceID        string    `json:"workspace_id"`
	ProjectID          string    `json:"project_id"`
	StoryID            string    `json:"story_id"`
	ContractID         string    `json:"contract_id"`
	ContractName       string    `json:"contract_name"`
	Phase              string    `json:"phase"`
	Sequence           int       `json:"sequence"`
	Status             string    `json:"status"`
	ClaimedBySessionID string    `json:"claimed_by_session_id,omitempty"`
	ClaimedViaGrantID  string    `json:"claimed_via_grant_id,omitempty"`
	ClaimedAt          time.Time `json:"claimed_at,omitempty"`
	PlanLedgerID       string    `json:"plan_ledger_id,omitempty"`
	CloseLedgerID      string    `json:"close_ledger_id,omitempty"`
	RequiredForClose   bool      `json:"required_for_close"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// NewID returns a fresh contract_instance id in the canonical
// `ci_<8hex>` form.
func NewID() string {
	return fmt.Sprintf("ci_%s", uuid.NewString()[:8])
}
