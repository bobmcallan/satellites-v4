// Package document is the satellites-v4 document primitive: a type-
// discriminated row in SurrealDB with hash-based version bumping. v4 ships
// enough surface to back document_ingest_file + document_get; later epics
// attach more type-specific shapes (contracts, skills, principles, etc.).
package document

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"
)

// Document is the single schema, type-discriminated shape shared by every
// type of authored content in satellites-v4. Workspace + project fields are
// reserved for a later epic.
type Document struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	Type      string    `json:"type"`
	Body      string    `json:"body"`
	BodyHash  string    `json:"body_hash"`
	Status    string    `json:"status"` // "active" | "archived"
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HashBody returns a sha256 content hash prefixed with "sha256:"; used as
// the equality test for Upsert's idempotence check.
func HashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// InferType maps a filename to a human-readable document type. The walking
// skeleton only cares about architecture + design + development docs; later
// types (principle, contract, skill, reviewer) override this via explicit
// type params at creation time.
func InferType(filename string) string {
	base := strings.ToLower(filepath.Base(filename))
	switch base {
	case "architecture.md":
		return "architecture"
	case "ui-design.md":
		return "design"
	case "development.md":
		return "development"
	default:
		return "document"
	}
}
