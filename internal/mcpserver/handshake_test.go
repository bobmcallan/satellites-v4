// sty_e1ab884d — the MCP server's handshake instructions block must
// be sourced from the seeded agent-process artifact when available,
// falling back to HandshakeFallbackInstructions when the resolver
// returns empty. These tests pin both branches.
//
// sty_4db0e025 slice A11: the resolver moved onto the typed client
// surface (*client.Client.ResolveHandshakeInstructions); the tests
// now construct a *client.Client directly so they remain wire-shape
// agnostic.
package mcpserver

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/agentprocess"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
)

func handshakeClient(docs document.Store) *client.Client {
	return client.New(client.Deps{Documents: docs})
}

func TestResolveHandshakeInstructions_FallsBackWhenEmpty(t *testing.T) {
	t.Parallel()
	got := handshakeClient(nil).ResolveHandshakeInstructions(context.Background(), HandshakeFallbackInstructions)
	if got != HandshakeFallbackInstructions {
		t.Errorf("nil docs handshake = %q, want fallback %q", got, HandshakeFallbackInstructions)
	}

	emptyStore := document.NewMemoryStore()
	got = handshakeClient(emptyStore).ResolveHandshakeInstructions(context.Background(), HandshakeFallbackInstructions)
	if got != HandshakeFallbackInstructions {
		t.Errorf("empty store handshake = %q, want fallback %q", got, HandshakeFallbackInstructions)
	}
}

func TestResolveHandshakeInstructions_ServesSeededBody(t *testing.T) {
	t.Parallel()
	store := document.NewMemoryStore()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	if _, err := configseed.Run(context.Background(), store, seedDir, "wksp_a", "system", time.Now().UTC()); err != nil {
		t.Fatalf("configseed Run: %v", err)
	}
	got := handshakeClient(store).ResolveHandshakeInstructions(context.Background(), HandshakeFallbackInstructions)
	if got == HandshakeFallbackInstructions {
		t.Errorf("seeded handshake fell through to fallback")
	}
	if got == "" {
		t.Errorf("seeded handshake = empty, want seeded body")
	}
	doc, err := store.GetByName(context.Background(), "", agentprocess.SystemDefaultName, nil)
	if err != nil {
		t.Fatalf("system default artifact not seeded: %v", err)
	}
	if got != doc.Body {
		t.Errorf("handshake body diverges from seeded artifact body")
	}
}
