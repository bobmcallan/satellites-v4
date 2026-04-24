package contract

import (
	"errors"
	"testing"
)

func TestDefaultWorkflowSpec_ValidDefaults(t *testing.T) {
	t.Parallel()
	spec := DefaultWorkflowSpec()
	if len(spec.Slots) != 4 {
		t.Fatalf("default slots: got %d want 4", len(spec.Slots))
	}
	names := []string{"preplan", "plan", "develop", "story_close"}
	for i, want := range names {
		if spec.Slots[i].ContractName != want {
			t.Fatalf("slot %d: got %q want %q", i, spec.Slots[i].ContractName, want)
		}
	}
}

func TestWorkflowSpec_Validate(t *testing.T) {
	t.Parallel()
	spec := DefaultWorkflowSpec()

	cases := []struct {
		name     string
		proposed []string
		wantKind string
	}{
		{"happy_path", []string{"preplan", "plan", "develop", "story_close"}, ""},
		{"multiple_develop_within_max", []string{"preplan", "plan", "develop", "develop", "story_close"}, ""},
		{"missing_preplan", []string{"plan", "develop", "story_close"}, "missing_required_slot"},
		{"missing_story_close", []string{"preplan", "plan", "develop"}, "missing_required_slot"},
		{"too_many_story_close", []string{"preplan", "plan", "develop", "story_close", "story_close"}, "count_out_of_range"},
		{"unknown_contract", []string{"preplan", "plan", "develop", "bogus", "story_close"}, "unknown_contract"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := spec.Validate(c.proposed)
			if c.wantKind == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error kind=%q, got nil", c.wantKind)
			}
			var se *SpecError
			if !errors.As(err, &se) {
				t.Fatalf("expected *SpecError, got %T", err)
			}
			if se.Kind != c.wantKind {
				t.Fatalf("kind: got %q want %q (%v)", se.Kind, c.wantKind, err)
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("expected errors.Is(err, ErrInvalidSpec), err=%v", err)
			}
		})
	}
}

func TestSpecError_Message(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  *SpecError
		want string
	}{
		{&SpecError{Kind: "missing_required_slot", ContractName: "preplan"}, `contract: missing required slot "preplan"`},
		{&SpecError{Kind: "count_out_of_range", ContractName: "develop", Count: 11, Min: 1, Max: 10}, `contract: "develop" count 11 out of range [1..10]`},
		{&SpecError{Kind: "unknown_contract", ContractName: "bogus"}, `contract: unknown contract name "bogus"`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.err.Kind, func(t *testing.T) {
			t.Parallel()
			if got := c.err.Error(); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
