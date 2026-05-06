// MCP catalogue page handler (sty_cd8b89c6). Renders the satellites
// MCP server's registered tool catalogue from the boot snapshot
// artifact (kind:mcp-catalogue, name=mcp-catalogue, system scope).
//
// Source of truth: the artifact body. The page is a 1:1 view of what
// Claude reads via tools/list — name, description, parameters. No
// embellishment, no operator-only annotations. Anything richer goes
// into the verb's description string in internal/mcpserver/, so both
// Claude and the page see it.
package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/mcpserver"
)

const otherPrimitive = "other"

type mcpPageData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Composite       mcpComposite
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	GlobalAdminChip bool
	IsGlobalAdmin   bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

type mcpComposite struct {
	HasSnapshot bool
	SnapshotAt  string
	ToolCount   int
	// SizeBytes is the size of the tools-array projection that
	// reaches Claude via tools/list. Approximates the per-turn cost
	// of the catalogue's instruction surface.
	SizeBytes int
	// SizeKB is SizeBytes formatted as "X.X" KB for display.
	SizeKB string
	// EstTokens is a rough token estimate using the standard
	// bytes/4 heuristic. Conservative — actual tokeniser output
	// varies ±20% on prose-heavy text.
	EstTokens int
	Groups    []mcpGroup
}

type mcpGroup struct {
	Primitive string
	Tools     []mcpToolRow
}

type mcpToolRow struct {
	Name        string
	Description string
	Parameters  []mcpParamRow
}

type mcpParamRow struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

func (p *Portal) handleMCPCatalogue(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, memberships := p.activeWorkspace(r, user)
	composite := buildMCPComposite(r.Context(), p.documents)
	data := mcpPageData{
		Title:           buildPageTitle(active, "", "mcp"),
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Composite:       composite,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		GlobalAdminChip: p.globalAdminChip(user, active, memberships),
		IsGlobalAdmin:   p.isGlobalAdmin(user),
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: r.URL.RequestURI(),
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "mcp.html", data); err != nil {
		p.logger.Error().Str("template", "mcp.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// buildMCPComposite reads the boot-snapshot artifact and projects it
// into the page-ready shape. Returns an empty-state composite when the
// artifact is missing, malformed, or the document store is nil — the
// page renders a "no snapshot yet" panel rather than 500.
//
// The catalogue is a system-scope artifact: every operator sees the
// same view of what Claude reads. Read with nil memberships to bypass
// workspace scoping (matching the established system-scope read
// pattern used by help_view.go for type=help docs). Without this, an
// operator whose workspace memberships do not include the system
// workspace would see the empty-state copy in production.
func buildMCPComposite(ctx context.Context, docs document.Store) mcpComposite {
	if docs == nil {
		return mcpComposite{}
	}
	doc, err := docs.GetByName(ctx, "", mcpserver.CatalogueArtifactName, nil)
	if err != nil || doc.Type != document.TypeArtifact || doc.Status != document.StatusActive {
		return mcpComposite{}
	}
	if !hasTag(doc.Tags, mcpserver.CatalogueKindTag) {
		return mcpComposite{}
	}
	var cat mcpserver.Catalogue
	if err := json.Unmarshal([]byte(doc.Body), &cat); err != nil {
		return mcpComposite{}
	}
	size := catalogueSurfaceSize(cat.Tools)
	return mcpComposite{
		HasSnapshot: true,
		SnapshotAt:  cat.SnapshotAt.UTC().Format(time.RFC3339),
		ToolCount:   len(cat.Tools),
		SizeBytes:   size,
		SizeKB:      formatKB(size),
		EstTokens:   size / 4,
		Groups:      groupCatalogue(cat.Tools),
	}
}

// catalogueSurfaceSize returns the byte length of the tools-array
// JSON projection — the shape Claude reads on every turn via
// tools/list. Excludes the SnapshotAt wrapper field which only the
// portal sees. Marshalling errors yield 0 (the page header simply
// hides the figure rather than render a misleading number).
func catalogueSurfaceSize(tools []mcpserver.CatalogueEntry) int {
	body, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	return len(body)
}

func formatKB(bytes int) string {
	kb := float64(bytes) / 1024.0
	switch {
	case kb < 10:
		return fmtFloat(kb, 2)
	case kb < 100:
		return fmtFloat(kb, 1)
	default:
		return fmtFloat(kb, 0)
	}
}

func fmtFloat(v float64, decimals int) string {
	switch decimals {
	case 0:
		return strconv.Itoa(int(v + 0.5))
	case 1:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// groupCatalogue partitions tools by primitive prefix. The prefix is
// the substring before the first underscore in the tool name. A tool
// without an underscore ("satellites_info" → "satellites" so this
// rarely fires; pure single-token verbs land in the "other" bucket).
// Grouping is a UI sort only — names and descriptions are emitted as
// registered.
func groupCatalogue(tools []mcpserver.CatalogueEntry) []mcpGroup {
	if len(tools) == 0 {
		return nil
	}
	buckets := map[string][]mcpToolRow{}
	for _, t := range tools {
		key := primitiveOf(t.Name)
		buckets[key] = append(buckets[key], mcpToolRow{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  paramsView(t.Parameters),
		})
	}
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]mcpGroup, 0, len(keys))
	for _, k := range keys {
		rows := buckets[k]
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
		out = append(out, mcpGroup{Primitive: k, Tools: rows})
	}
	return out
}

func primitiveOf(toolName string) string {
	if idx := strings.IndexByte(toolName, '_'); idx > 0 {
		return toolName[:idx]
	}
	return otherPrimitive
}

func paramsView(in []mcpserver.CatalogueParam) []mcpParamRow {
	out := make([]mcpParamRow, 0, len(in))
	for _, p := range in {
		out = append(out, mcpParamRow{
			Name:        p.Name,
			Type:        p.Type,
			Required:    p.Required,
			Description: p.Description,
		})
	}
	return out
}
