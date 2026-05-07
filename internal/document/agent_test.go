package document

import (
	"strings"
	"testing"
)

func stringPtr(s string) *string { return &s }

func TestAgentSettings_MarshalRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   AgentSettings
	}{
		{"empty", AgentSettings{}},
		{"with permission_patterns", AgentSettings{PermissionPatterns: []string{"Read:**"}}},
		{"with skill_refs", AgentSettings{SkillRefs: []string{"doc_sk_x"}}},
		{"ephemeral", AgentSettings{Ephemeral: true, StoryID: stringPtr("sty_abc")}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload, err := MarshalAgentSettings(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got, err := UnmarshalAgentSettings(payload)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if len(got.PermissionPatterns) != len(tc.in.PermissionPatterns) {
				t.Errorf("permission_patterns len = %d, want %d", len(got.PermissionPatterns), len(tc.in.PermissionPatterns))
			}
			if len(got.SkillRefs) != len(tc.in.SkillRefs) {
				t.Errorf("skill_refs len = %d, want %d", len(got.SkillRefs), len(tc.in.SkillRefs))
			}
			if got.Ephemeral != tc.in.Ephemeral {
				t.Errorf("ephemeral mismatch: got %v want %v", got.Ephemeral, tc.in.Ephemeral)
			}
		})
	}
}

// TestAgentSettings_NoRequiresReviewField locks AC #1 of sty_9f3562b8:
// the substrate-side review-pairing flag is gone. Marshalling a
// populated AgentSettings must not emit a `requires_review` JSON key.
func TestAgentSettings_NoRequiresReviewField(t *testing.T) {
	t.Parallel()
	settings := AgentSettings{
		PermissionPatterns: []string{"Read:**"},
		SkillRefs:          []string{"doc_sk_x"},
		Ephemeral:          true,
		StoryID:            stringPtr("sty_abc"),
		Delivers:           []string{"contract:develop"},
		Reviews:            []string{"contract:plan"},
	}
	payload, err := MarshalAgentSettings(settings)
	if err != nil {
		t.Fatalf("MarshalAgentSettings: %v", err)
	}
	if strings.Contains(string(payload), "requires_review") {
		t.Fatalf("payload contains requires_review: %s", payload)
	}
}

func TestUnmarshalAgentSettings_EmptyPayloadReturnsZero(t *testing.T) {
	t.Parallel()
	got, err := UnmarshalAgentSettings(nil)
	if err != nil {
		t.Fatalf("nil payload: %v", err)
	}
	if got.Ephemeral || len(got.PermissionPatterns) != 0 || len(got.SkillRefs) != 0 {
		t.Errorf("expected zero value; got %+v", got)
	}
	got2, err := UnmarshalAgentSettings([]byte{})
	if err != nil {
		t.Fatalf("empty payload: %v", err)
	}
	if got2.Ephemeral || len(got2.PermissionPatterns) != 0 || len(got2.SkillRefs) != 0 {
		t.Errorf("expected zero value; got %+v", got2)
	}
}
