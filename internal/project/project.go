// Package project is the satellites-v4 project primitive: the top-level
// container within a workspace that every other primitive (documents,
// stories, tasks, ledger rows, repo references) scopes to. Workspace
// scoping is reserved for a later epic.
package project

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status values for a Project. Write-once Active today; Archive is reserved
// for a later archive-story if/when it lands.
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

// Project is the top-level primitive within a workspace. Every other v4
// primitive row carries a ProjectID scoping back to one of these rows.
type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	OwnerUserID string    `json:"owner_user_id"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NewID returns a fresh project id in the canonical `proj_<8hex>` form.
func NewID() string {
	return fmt.Sprintf("proj_%s", uuid.NewString()[:8])
}
