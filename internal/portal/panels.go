// Panel registry for the project page (sty_70c0f7a3). One ordered list
// declares the panels rendered on /projects/{id}; project_detail.html
// iterates this slice. Adding a panel = one entry here + one
// _panel_<id>.html sub-template.
//
// URL is the source of truth for which panels are open: `?expand=` lists
// the open keys. When the param is absent, DefaultExpanded applies. The
// front-end Alpine store reads/writes the URL — no server-side state.
package portal

// panel is one entry in the project page registry.
type panel struct {
	ID              string
	Title           string
	DefaultExpanded bool
}

// defaultPanels returns the project page panel registry. Order is
// render order top-to-bottom.
func defaultPanels() []panel {
	return []panel{
		{ID: "meta", Title: "project", DefaultExpanded: true},
		{ID: "stories", Title: "stories", DefaultExpanded: true},
		{ID: "documents", Title: "documents", DefaultExpanded: true},
		{ID: "contracts", Title: "contracts", DefaultExpanded: false},
		{ID: "configuration", Title: "configuration", DefaultExpanded: false},
		{ID: "repo", Title: "repo", DefaultExpanded: false},
		{ID: "changelog", Title: "changelog", DefaultExpanded: false},
		// sty_9596b012: ledger panel removed. The /projects/{id}/ledger
		// page is the canonical surface (filters + timeline) and the
		// nav link from sty_5b92165a routes there directly.
	}
}
