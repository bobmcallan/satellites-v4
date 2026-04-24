package contract

import (
	"errors"
	"fmt"
)

// Slot is one entry in a project's workflow_spec. A spec lists every
// contract_name a story's workflow may include, along with min/max
// counts and whether the slot is required for a valid claim.
type Slot struct {
	ContractName string `json:"contract_name"`
	Required     bool   `json:"required"`
	MinCount     int    `json:"min_count"`
	MaxCount     int    `json:"max_count"`
	Source       string `json:"source"`
}

// WorkflowSpec is a project's ordered list of contract slots.
type WorkflowSpec struct {
	Slots []Slot `json:"slots"`
}

// DefaultWorkflowSpec is used when a project has not set one. Mirrors
// the v3 default: preplan → plan → develop (1-10) → story_close.
func DefaultWorkflowSpec() WorkflowSpec {
	return WorkflowSpec{Slots: []Slot{
		{ContractName: "preplan", Required: true, MinCount: 1, MaxCount: 1, Source: "system"},
		{ContractName: "plan", Required: true, MinCount: 1, MaxCount: 1, Source: "system"},
		{ContractName: "develop", Required: true, MinCount: 1, MaxCount: 10, Source: "system"},
		{ContractName: "story_close", Required: true, MinCount: 1, MaxCount: 1, Source: "system"},
	}}
}

// SpecError is a structured validation failure from WorkflowSpec.Validate.
// Error returns a low-cardinality message; callers inspect the fields.
type SpecError struct {
	Kind         string // missing_required_slot | count_out_of_range | unknown_contract
	ContractName string
	Count        int
	Min          int
	Max          int
}

// Error implements error. Message is stable per Kind.
func (e *SpecError) Error() string {
	switch e.Kind {
	case "missing_required_slot":
		return fmt.Sprintf("contract: missing required slot %q", e.ContractName)
	case "count_out_of_range":
		return fmt.Sprintf("contract: %q count %d out of range [%d..%d]", e.ContractName, e.Count, e.Min, e.Max)
	case "unknown_contract":
		return fmt.Sprintf("contract: unknown contract name %q", e.ContractName)
	default:
		return "contract: spec validation failed"
	}
}

// ErrInvalidSpec wraps SpecError for errors.Is discrimination at
// call sites that don't need the structured fields.
var ErrInvalidSpec = errors.New("contract: workflow spec violation")

// Is makes a SpecError match errors.Is(err, ErrInvalidSpec).
func (e *SpecError) Is(target error) bool { return target == ErrInvalidSpec }

// Validate checks that proposed (a list of contract_names) satisfies
// spec. Returns nil on success. Error sentinels surface via *SpecError
// so handlers can render structured responses.
func (s WorkflowSpec) Validate(proposed []string) error {
	counts := make(map[string]int, len(proposed))
	known := make(map[string]Slot, len(s.Slots))
	for _, slot := range s.Slots {
		known[slot.ContractName] = slot
	}
	for _, name := range proposed {
		if _, ok := known[name]; !ok {
			return &SpecError{Kind: "unknown_contract", ContractName: name}
		}
		counts[name]++
	}
	for _, slot := range s.Slots {
		n := counts[slot.ContractName]
		if slot.Required && n < slot.MinCount {
			if n == 0 {
				return &SpecError{Kind: "missing_required_slot", ContractName: slot.ContractName}
			}
			return &SpecError{Kind: "count_out_of_range", ContractName: slot.ContractName, Count: n, Min: slot.MinCount, Max: slot.MaxCount}
		}
		if n > slot.MaxCount {
			return &SpecError{Kind: "count_out_of_range", ContractName: slot.ContractName, Count: n, Min: slot.MinCount, Max: slot.MaxCount}
		}
	}
	return nil
}
