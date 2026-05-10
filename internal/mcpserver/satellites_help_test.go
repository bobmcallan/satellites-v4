package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestSatellitesHelp_ReturnsCatalogue(t *testing.T) {
	s := &Server{}
	res, err := s.handleSatellitesHelp(context.Background(), mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleSatellitesHelp: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected non-error result, got: %+v", res)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	var cat helpCatalogue
	if err := json.Unmarshal([]byte(text), &cat); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, text)
	}
	if cat.Binary != "satellites-client" {
		t.Errorf("Binary = %q, want %q", cat.Binary, "satellites-client")
	}
	if got := len(cat.Nouns); got != CLICatalogueNounCount {
		t.Errorf("nouns count = %d, want %d (drift from cmd/satellites-client/nouns.go)", got, CLICatalogueNounCount)
	}
	// Spot-check the agent-critical surface: every read verb the
	// dispatched-Claude prompt template depends on must appear.
	mustHave := map[string][]string{
		"task":      {"add", "get", "claim", "update", "walk", "plan"},
		"story":     {"get", "update-status", "field-set"},
		"ledger":    {"append", "get", "list", "search"},
		"project":   {"set"},
		"agent":     {"get"},
		"contract":  {"get"},
		"principle": {"list"},
		"session":   {"whoami"},
	}
	verbsByNoun := map[string]map[string]bool{}
	for _, n := range cat.Nouns {
		verbsByNoun[n.Name] = map[string]bool{}
		for _, v := range n.Verbs {
			verbsByNoun[n.Name][v.Name] = true
		}
	}
	for noun, verbs := range mustHave {
		for _, v := range verbs {
			if !verbsByNoun[noun][v] {
				t.Errorf("catalogue missing %s %s", noun, v)
			}
		}
	}
	// Notes must include the persistent-flag enumeration so IDE
	// agents can render --json/--compact/etc. without round-trip.
	gotNotes := strings.Join(cat.Notes, "\n")
	for _, want := range []string{"--json", "--compact", "Auto-JSON", "exit codes"} {
		if !strings.Contains(gotNotes, want) {
			t.Errorf("notes missing %q\nfull notes:\n%s", want, gotNotes)
		}
	}
}

// TestSatellitesHelp_NounCountConstantMatches asserts the exported
// constant stays in lock-step with the catalogue's actual length —
// catches drift when a maintainer adds a noun to cliCatalogue but
// forgets to bump CLICatalogueNounCount.
func TestSatellitesHelp_NounCountConstantMatches(t *testing.T) {
	if got := len(cliCatalogue); got != CLICatalogueNounCount {
		t.Fatalf("len(cliCatalogue) = %d but CLICatalogueNounCount = %d — bump the constant", got, CLICatalogueNounCount)
	}
}
