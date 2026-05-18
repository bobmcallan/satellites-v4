// sty_7667c9bc — shared realtime bridge. defaultRealtimeRoutes is the
// single source-of-truth for the kind → entity table embedded in
// head.html as window.SATELLITES_REALTIME_ROUTES. The bridge in
// pages/static/realtime_bridge.js matches an incoming hub event's Kind
// against the longest kind_prefix; the first match wins and the bridge
// dispatches a satellites:realtime:<entity> CustomEvent.
//
// Adding a panel = one entry here. No new HTTP endpoint required (the
// route table is build-time, not per-tenant) — see plan §2.2 rationale.
package portal

import (
	"encoding/json"
	"html/template"
)

// realtimeRoute maps a kind prefix to the entity name dispatched on
// the CustomEvent. The JSON keys are lowercase_snake to match what the
// bridge reads in window.SATELLITES_REALTIME_ROUTES.
type realtimeRoute struct {
	KindPrefix string `json:"kind_prefix"`
	Entity     string `json:"entity"`
}

// defaultRealtimeRoutes returns the static route table. The order is
// load-bearing: the bridge picks the first matching prefix, so longer
// prefixes (if added later) must precede their shorter ancestors.
func defaultRealtimeRoutes() []realtimeRoute {
	return []realtimeRoute{
		{KindPrefix: "story.", Entity: "story"},
		{KindPrefix: "task.", Entity: "task"},
		{KindPrefix: "ledger.", Entity: "ledger"},
		{KindPrefix: "document.", Entity: "document"},
		{KindPrefix: "contract.", Entity: "contract"},
		{KindPrefix: "repo.", Entity: "repo"},
		{KindPrefix: "project.", Entity: "project"},
	}
}

// realtimeRoutesJSON marshals defaultRealtimeRoutes() into a template.JS
// value safe for direct embedding inside a <script> tag. The result
// becomes the right-hand side of window.SATELLITES_REALTIME_ROUTES = …
// in head.html.
func realtimeRoutesJSON() template.JS {
	b, err := json.Marshal(defaultRealtimeRoutes())
	if err != nil {
		// json.Marshal on a slice of stringly-typed structs cannot
		// fail; on the impossible-error path emit the empty array so
		// the bridge degrades to "no routes" rather than a JS syntax
		// error inside the embedded literal.
		return template.JS("[]")
	}
	return template.JS(b)
}
