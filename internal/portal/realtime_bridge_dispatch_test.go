// sty_7667c9bc — assert the contract of pages/static/realtime_bridge.js
// at source level. The shared bridge:
//
//   1. constructs exactly one SatellitesWS instance per page;
//   2. routes inbound hub events through a kind_prefix → entity table
//      loaded from window.SATELLITES_REALTIME_ROUTES;
//   3. drops events whose payload.project_id is non-empty and not equal
//      to the page's data-project-id host attribute (single point of
//      truth — panels do not re-check scope);
//   4. dispatches a CustomEvent named `satellites:realtime:<entity>`
//      with `detail = {kind, payload, eventID}`.
//
// The tests below assert each clause by reading the JS source and
// grepping for the contract markers. A full goja runtime is overkill
// for a contract this small; the chromedp e2e in tests/portalui/
// covers the live dispatch path against a real browser.
package portal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRealtimeBridgeJS(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	path := filepath.Join(root, "pages", "static", "realtime_bridge.js")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read realtime_bridge.js (%s): %v", path, err)
	}
	return string(raw)
}

// TestRealtimeBridge_SingleWSConstructor — the bridge is the single
// owner of the page's SatellitesWS connection. Per pr_no_unrequested_compat
// adding a second call site here would re-introduce the multi-WS
// problem this story is designed to remove.
func TestRealtimeBridge_SingleWSConstructor(t *testing.T) {
	t.Parallel()
	src := readRealtimeBridgeJS(t)
	count := strings.Count(src, "new window.SatellitesWS(")
	if count != 1 {
		t.Errorf("realtime_bridge.js has %d `new window.SatellitesWS(` call sites; want exactly 1", count)
	}
	if strings.Count(src, "new SatellitesWS(") != 0 {
		// The wsIndicator uses the unqualified form inside ws.js; the
		// bridge must qualify with `window.` so the test above counts
		// reliably.
		t.Errorf("realtime_bridge.js must qualify the constructor as window.SatellitesWS to keep the single-call-site invariant grepable")
	}
}

// TestRealtimeBridge_RouteTableConsulted — the bridge resolves an
// event's entity by walking the route table from
// window.SATELLITES_REALTIME_ROUTES. Without this lookup, the bridge
// could not dispatch `satellites:realtime:<entity>` CustomEvents.
func TestRealtimeBridge_RouteTableConsulted(t *testing.T) {
	t.Parallel()
	src := readRealtimeBridgeJS(t)
	if !strings.Contains(src, "window.SATELLITES_REALTIME_ROUTES") {
		t.Errorf("bridge does not consult window.SATELLITES_REALTIME_ROUTES; route table is dead")
	}
	if !strings.Contains(src, "kind_prefix") {
		t.Errorf("bridge route lookup missing kind_prefix; the contract with the embedded JSON is broken")
	}
	// First-prefix-wins lookup.
	if !strings.Contains(src, "kind.indexOf(r.kind_prefix) === 0") {
		t.Errorf("bridge prefix match must use indexOf-at-0 to mirror the JSON contract documented in plan §2.3")
	}
}

// TestRealtimeBridge_OffProjectDrop — the bridge drops events whose
// payload.project_id is set AND does not match the page's
// data-project-id host attribute. The per-panel scope guard is removed
// in sty_7667c9bc; a single point of truth lives here.
func TestRealtimeBridge_OffProjectDrop(t *testing.T) {
	t.Parallel()
	src := readRealtimeBridgeJS(t)
	if !strings.Contains(src, "data-project-id") {
		t.Errorf("bridge does not read the data-project-id host attribute; off-project events will leak to panels")
	}
	if !strings.Contains(src, "eventProject !== this._projectID") {
		t.Errorf("bridge missing the off-project drop check (eventProject !== this._projectID); panels will patch foreign-project rows")
	}
}

// TestRealtimeBridge_CustomEventShape — the dispatch must use the
// canonical `satellites:realtime:` + entity name with a
// `{kind, payload, eventID}` detail shape. The reviewer rejects any
// deviation from this contract (see review-criteria ldg_ce1d8b37).
func TestRealtimeBridge_CustomEventShape(t *testing.T) {
	t.Parallel()
	src := readRealtimeBridgeJS(t)
	if !strings.Contains(src, "new CustomEvent('satellites:realtime:'") {
		t.Errorf("bridge does not dispatch a CustomEvent named satellites:realtime:<entity>")
	}
	if !strings.Contains(src, "detail: { kind:") {
		t.Errorf("bridge CustomEvent detail must lead with kind field")
	}
	if !strings.Contains(src, "payload: payload") {
		t.Errorf("bridge CustomEvent detail must include payload field")
	}
	if !strings.Contains(src, "eventID: ev.ID") {
		t.Errorf("bridge CustomEvent detail must include eventID field (for panel de-dup)")
	}
	if !strings.Contains(src, "document.dispatchEvent(") {
		t.Errorf("bridge must dispatch on document so per-panel listeners receive the event")
	}
}

// TestRealtimeBridge_DefaultRoutesContract — defaultRealtimeRoutes is
// the canonical source of the JSON blob embedded in head.html. The
// test asserts the seven entries (in plan-order) are present so a
// future edit cannot drop an entity without explicitly updating this
// expectation.
func TestRealtimeBridge_DefaultRoutesContract(t *testing.T) {
	t.Parallel()
	routes := defaultRealtimeRoutes()
	want := []realtimeRoute{
		{KindPrefix: "story.", Entity: "story"},
		{KindPrefix: "task.", Entity: "task"},
		{KindPrefix: "ledger.", Entity: "ledger"},
		{KindPrefix: "document.", Entity: "document"},
		{KindPrefix: "contract.", Entity: "contract"},
		{KindPrefix: "repo.", Entity: "repo"},
		{KindPrefix: "project.", Entity: "project"},
	}
	if len(routes) != len(want) {
		t.Fatalf("defaultRealtimeRoutes length = %d, want %d", len(routes), len(want))
	}
	for i := range want {
		if routes[i] != want[i] {
			t.Errorf("route[%d] = %+v, want %+v", i, routes[i], want[i])
		}
	}
	// The JSON-encoded form is what head.html embeds. The bridge
	// reads the same shape — assert both the field names and the
	// presence of every prefix in the marshalled blob.
	blob := string(realtimeRoutesJSON())
	for _, w := range want {
		needle := `"kind_prefix":"` + w.KindPrefix + `","entity":"` + w.Entity + `"`
		if !strings.Contains(blob, needle) {
			t.Errorf("realtimeRoutesJSON missing entry %q; embedded blob diverged from struct", needle)
		}
	}
}

// TestRealtimeBridge_LedgerViewMigrated — ledger_view.js no longer
// owns its own WS. The migration to the shared bridge replaces
// attachWS + the SatellitesWS constructor with a CustomEvent listener.
func TestRealtimeBridge_LedgerViewMigrated(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	raw, err := os.ReadFile(filepath.Join(root, "pages", "static", "ledger_view.js"))
	if err != nil {
		t.Fatalf("read ledger_view.js: %v", err)
	}
	js := string(raw)
	if strings.Contains(js, "attachWS()") {
		t.Errorf("ledger_view.js still calls attachWS(); sty_7667c9bc moves the WS owner to realtime_bridge.js")
	}
	if strings.Contains(js, "new window.SatellitesWS(") {
		t.Errorf("ledger_view.js still constructs SatellitesWS; bridge owns the connection")
	}
	if !strings.Contains(js, `addEventListener('satellites:realtime:ledger'`) {
		t.Errorf("ledger_view.js missing satellites:realtime:ledger listener; live tail will not patch")
	}
}
