package configseed

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestContractToInput_ReviewRequired_Present_True covers the
// pr_review_required_gate parser lint: a contract carrying
// `review_required: true` round-trips into the Structured payload.
// Sty_f49c378d.
func TestContractToInput_ReviewRequired_Present_True(t *testing.T) {
	t.Parallel()
	fm := Frontmatter{
		"name":             "test_contract_true",
		"category":         "develop",
		"validation_mode":  "llm",
		"review_required": true,
	}
	in, err := contractToInput(fm, []byte("body"), "wksp_sys", "system")
	if err != nil {
		t.Fatalf("contractToInput: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(in.Structured, &payload); err != nil {
		t.Fatalf("decode Structured: %v", err)
	}
	got, ok := payload["review_required"].(bool)
	if !ok {
		t.Fatalf("review_required not bool in Structured payload: %T %v", payload["review_required"], payload["review_required"])
	}
	if !got {
		t.Errorf("review_required = %v, want true", got)
	}
}

// TestContractToInput_ReviewRequired_Present_False covers the survival
// of `review_required: false` through the marshal step — pruneEmpty
// must not drop it; the false carries through. The failure mode this
// guards against is a silent re-enable of the gate's bypass (a contract
// that should opt in would appear to opt out if false leaked through
// as missing).
func TestContractToInput_ReviewRequired_Present_False(t *testing.T) {
	t.Parallel()
	fm := Frontmatter{
		"name":             "test_contract_false",
		"category":         "commit",
		"validation_mode":  "llm",
		"review_required": false,
	}
	in, err := contractToInput(fm, []byte("body"), "wksp_sys", "system")
	if err != nil {
		t.Fatalf("contractToInput: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(in.Structured, &payload); err != nil {
		t.Fatalf("decode Structured: %v", err)
	}
	raw, present := payload["review_required"]
	if !present {
		t.Fatalf("review_required key missing from Structured payload — pruneEmpty must not drop false")
	}
	got, ok := raw.(bool)
	if !ok {
		t.Fatalf("review_required not bool: %T %v", raw, raw)
	}
	if got {
		t.Errorf("review_required = %v, want false", got)
	}
}

// TestContractToInput_ReviewRequired_Absent rejects a contract that
// omits `review_required` from its frontmatter. The substrate refuses
// to seed the contract — the choice must be explicit at author time.
func TestContractToInput_ReviewRequired_Absent(t *testing.T) {
	t.Parallel()
	fm := Frontmatter{
		"name":            "test_contract_missing",
		"category":        "plan",
		"validation_mode": "llm",
	}
	_, err := contractToInput(fm, []byte("body"), "wksp_sys", "system")
	if err == nil {
		t.Fatal("contractToInput accepted missing review_required, want error")
	}
	if !strings.Contains(err.Error(), "review_required required") {
		t.Errorf("error = %q, want substring 'review_required required'", err.Error())
	}
}

// TestContractToInput_ReviewRequired_NonBool rejects a non-bool value
// (string, int, slice). YAML coerces literal `true`/`false` to bool but
// `"true"`/`"false"` stays a string — the parser must reject the latter
// to keep the gate's wiring honest.
func TestContractToInput_ReviewRequired_NonBool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"string", "true"},
		{"int", 1},
		{"slice", []any{"true"}},
		{"nil", nil},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fm := Frontmatter{
				"name":             "test_contract_nonbool_" + tc.name,
				"category":         "plan",
				"validation_mode":  "llm",
				"review_required": tc.value,
			}
			_, err := contractToInput(fm, []byte("body"), "wksp_sys", "system")
			if err == nil {
				t.Fatalf("contractToInput accepted review_required=%v (%T), want error", tc.value, tc.value)
			}
			if !strings.Contains(err.Error(), "review_required required") {
				t.Errorf("error = %q, want substring 'review_required required'", err.Error())
			}
		})
	}
}
