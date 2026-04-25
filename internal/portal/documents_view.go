// Documents browser composite for slice 11.4 (story_5bc06738). Type
// tabs + cards + detail per docs/ui-design.md §2.5. Builds the page
// composite from document.Store.List + Search and the per-doc detail
// composite from GetByID + linked-stories scan.
package portal

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/story"
)

const documentsDefaultLimit = 100

// documentsComposite is the view-model for the list page.
type documentsComposite struct {
	Cards   []documentCard  `json:"cards"`
	Filters documentFilters `json:"filters"`
	Total   int             `json:"total"`
}

// documentDetailComposite is the view-model for the detail page.
type documentDetailComposite struct {
	Document       documentCard   `json:"document"`
	Body           string         `json:"body"`
	Structured     string         `json:"structured,omitempty"`
	LinkedStories  []linkedStory  `json:"linked_stories"`
	VersionHistory []versionEntry `json:"version_history"`
}

type documentCard struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Scope     string   `json:"scope"`
	Name      string   `json:"name"`
	Tags      []string `json:"tags,omitempty"`
	Version   int      `json:"version"`
	Status    string   `json:"status"`
	UpdatedAt string   `json:"updated_at"`
}

type documentFilters struct {
	Type  string `json:"type,omitempty"`
	Query string `json:"query,omitempty"`
	Sort  string `json:"sort,omitempty"`
}

type linkedStory struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// versionDetailView is the view-model for the per-version detail
// page. Carries the historical body alongside the live document's
// metadata so the template can render the diff context inline.
type versionDetailView struct {
	Version    int    `json:"version"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	UpdatedBy  string `json:"updated_by,omitempty"`
	BodyHash   string `json:"body_hash,omitempty"`
	Body       string `json:"body"`
	Structured string `json:"structured,omitempty"`
}

// versionDetailFromRow projects a DocumentVersion into the view-model
// shape consumed by document_version_detail.html.
func versionDetailFromRow(v document.DocumentVersion) versionDetailView {
	out := versionDetailView{
		Version:   v.Version,
		UpdatedAt: v.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedBy: v.UpdatedBy,
		BodyHash:  v.BodyHash,
		Body:      v.Body,
	}
	if len(v.Structured) > 0 {
		out.Structured = string(v.Structured)
	}
	return out
}

// versionEntry is one prior version of a document, populated from
// document.Store.ListVersions. DiffHref points at the per-version
// detail route so the user can compare against the live document.
type versionEntry struct {
	Version   int    `json:"version"`
	UpdatedAt string `json:"updated_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
	BodyHash  string `json:"body_hash,omitempty"`
	DiffHref  string `json:"diff_href,omitempty"`
}

// parseDocumentFilters reads `?type=`, `?q=`, `?sort=` from the request.
func parseDocumentFilters(r *http.Request) documentFilters {
	q := r.URL.Query()
	return documentFilters{
		Type:  strings.TrimSpace(q.Get("type")),
		Query: strings.TrimSpace(q.Get("q")),
		Sort:  strings.TrimSpace(q.Get("sort")),
	}
}

// buildDocumentsComposite assembles the list-page composite.
func buildDocumentsComposite(ctx context.Context, store document.Store, f documentFilters, memberships []string) documentsComposite {
	if store == nil {
		return documentsComposite{Filters: f}
	}
	listOpts := document.ListOptions{
		Type:  f.Type,
		Limit: documentsDefaultLimit,
	}
	rows, err := store.List(ctx, listOpts, memberships)
	if err != nil {
		return documentsComposite{Filters: f}
	}
	if f.Query != "" {
		rows = filterByQuery(rows, f.Query)
	}
	switch f.Sort {
	case "name_asc":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	default:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })
	}
	cards := make([]documentCard, 0, len(rows))
	for _, d := range rows {
		cards = append(cards, documentCardFor(d))
	}
	return documentsComposite{Cards: cards, Filters: f, Total: len(cards)}
}

// buildDocumentDetail assembles the per-document detail composite,
// including linked stories (filtered from story.List by source: tag
// matching the document name).
func buildDocumentDetail(ctx context.Context, store document.Store, stories story.Store, projectID, documentID string, memberships []string) (documentDetailComposite, error) {
	d, err := store.GetByID(ctx, documentID, memberships)
	if err != nil {
		return documentDetailComposite{}, err
	}
	out := documentDetailComposite{
		Document:      documentCardFor(d),
		Body:          d.Body,
		LinkedStories: linkedStoriesFor(ctx, stories, projectID, d.Name, memberships),
	}
	if len(d.Structured) > 0 {
		out.Structured = string(d.Structured)
	}
	out.VersionHistory = versionHistoryFor(ctx, store, d.ID, memberships)
	return out, nil
}

// versionHistoryFor calls Store.ListVersions and shapes the rows for
// the document_detail template. DiffHref points at the version-detail
// portal route so the user can compare a prior body against the live
// document. Returns an empty slice on error or when no prior versions
// exist.
func versionHistoryFor(ctx context.Context, store document.Store, documentID string, memberships []string) []versionEntry {
	rows, err := store.ListVersions(ctx, documentID, memberships)
	if err != nil || len(rows) == 0 {
		return []versionEntry{}
	}
	out := make([]versionEntry, 0, len(rows))
	for _, v := range rows {
		out = append(out, versionEntry{
			Version:   v.Version,
			UpdatedAt: v.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			UpdatedBy: v.UpdatedBy,
			BodyHash:  v.BodyHash,
			DiffHref:  fmt.Sprintf("/documents/%s/versions/%d", documentID, v.Version),
		})
	}
	return out
}

// documentCardFor projects a document.Document into the card shape.
func documentCardFor(d document.Document) documentCard {
	return documentCard{
		ID:        d.ID,
		Type:      d.Type,
		Scope:     d.Scope,
		Name:      d.Name,
		Tags:      d.Tags,
		Version:   d.Version,
		Status:    d.Status,
		UpdatedAt: d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// linkedStoriesFor finds stories whose `source:` tags reference docName.
// Mirrors the convention used by the story-view source-docs panel
// (story_3b450d9e).
func linkedStoriesFor(ctx context.Context, stories story.Store, projectID, docName string, memberships []string) []linkedStory {
	if stories == nil || projectID == "" || docName == "" {
		return []linkedStory{}
	}
	rows, err := stories.List(ctx, projectID, story.ListOptions{Limit: 200}, memberships)
	if err != nil {
		return []linkedStory{}
	}
	out := make([]linkedStory, 0)
	for _, s := range rows {
		for _, t := range s.Tags {
			if !strings.HasPrefix(t, "source:") {
				continue
			}
			raw := strings.TrimPrefix(t, "source:")
			path := raw
			if i := strings.IndexByte(raw, '#'); i >= 0 {
				path = raw[:i]
			}
			if path == docName {
				out = append(out, linkedStory{ID: s.ID, Title: s.Title})
				break
			}
		}
	}
	return out
}

// filterByQuery applies a case-insensitive substring filter on name +
// body for the in-process MemoryStore code path. The Surreal-backed
// SearchSemantic is the production query path; this fallback keeps the
// portal usable when only the in-memory store is wired (tests, dev
// without DB).
func filterByQuery(rows []document.Document, q string) []document.Document {
	q = strings.ToLower(q)
	out := make([]document.Document, 0, len(rows))
	for _, d := range rows {
		if strings.Contains(strings.ToLower(d.Name), q) || strings.Contains(strings.ToLower(d.Body), q) {
			out = append(out, d)
		}
	}
	return out
}
