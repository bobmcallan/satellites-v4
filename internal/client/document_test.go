package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/workspace"
)

func newDocClient(t *testing.T) (*Client, string, time.Time) {
	t.Helper()
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	docStore := document.NewMemoryStore()
	wsStore := workspace.NewMemoryStore()
	ws, err := wsStore.Create(context.Background(), "u_alice", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(context.Background(), ws.ID, "u_alice", workspace.RoleAdmin, "system", now))

	c := New(Deps{Documents: docStore, Workspaces: wsStore})
	return c, ws.ID, now
}

func TestDocumentGet_ByIDScopeSystemIsPublic(t *testing.T) {
	c, _, now := newDocClient(t)
	doc, err := c.deps.Documents.Create(context.Background(), document.Document{
		Type:   document.TypeContract,
		Scope:  document.ScopeSystem,
		Name:   "develop",
		Body:   "system contract",
		Status: document.StatusActive,
	}, now)
	require.NoError(t, err)

	got, err := c.DocumentGet(context.Background(), Caller{UserID: "u_outsider"}, DocumentGetInput{
		ID:          doc.ID,
		Memberships: []string{}, // non-nil empty: would deny tenant rows
	})
	require.NoError(t, err)
	assert.Equal(t, doc.ID, got.ID)
}

func TestDocumentGet_ByIDTenantNeedsMembership(t *testing.T) {
	c, wsID, now := newDocClient(t)
	doc, err := c.deps.Documents.Create(context.Background(), document.Document{
		WorkspaceID: wsID,
		Type:        document.TypeAgent,
		Scope:       document.ScopeWorkspace,
		Name:        "alpha_agent",
		Body:        "tenant agent",
		Status:      document.StatusActive,
	}, now)
	require.NoError(t, err)

	_, err = c.DocumentGet(context.Background(), Caller{UserID: "u_outsider"}, DocumentGetInput{
		ID:          doc.ID,
		Memberships: []string{"wksp_other"},
	})
	require.ErrorIs(t, err, document.ErrNotFound)
}

func TestDocumentGet_TypeMismatchErrors(t *testing.T) {
	c, _, now := newDocClient(t)
	doc, err := c.deps.Documents.Create(context.Background(), document.Document{
		Type:   document.TypeContract,
		Scope:  document.ScopeSystem,
		Name:   "develop",
		Status: document.StatusActive,
	}, now)
	require.NoError(t, err)

	_, err = c.DocumentGet(context.Background(), Caller{}, DocumentGetInput{
		ID:   doc.ID,
		Type: document.TypeAgent, // wrong
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type=")
}

func TestDocumentGet_NameResolvesSystemTier(t *testing.T) {
	c, _, now := newDocClient(t)
	_, err := c.deps.Documents.Create(context.Background(), document.Document{
		Type:   document.TypeContract,
		Scope:  document.ScopeSystem,
		Name:   "plan",
		Body:   "system plan contract",
		Status: document.StatusActive,
	}, now)
	require.NoError(t, err)

	got, err := c.DocumentGet(context.Background(), Caller{}, DocumentGetInput{
		Name: "plan",
		Type: document.TypeContract,
	})
	require.NoError(t, err)
	assert.Equal(t, "plan", got.Name)
}

func TestDocumentGet_RejectsMissingIDAndName(t *testing.T) {
	c, _, _ := newDocClient(t)
	_, err := c.DocumentGet(context.Background(), Caller{}, DocumentGetInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id or name")
}

func TestDocumentCreate_ProjectScopeStampsProjectID(t *testing.T) {
	c, wsID, now := newDocClient(t)
	got, err := c.DocumentCreate(context.Background(), Caller{UserID: "u_alice"}, DocumentCreateInput{
		Type: document.TypeAgent, Scope: document.ScopeProject, Name: "alpha_agent",
		Body: "tenant agent body", WorkspaceID: wsID, ResolvedProjectID: "proj_alpha", Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, document.ScopeProject, got.Scope)
	require.NotNil(t, got.ProjectID)
	assert.Equal(t, "proj_alpha", *got.ProjectID)
	assert.Equal(t, wsID, got.WorkspaceID)
}

func TestDocumentCreate_SystemScopeRejectsProjectID(t *testing.T) {
	c, _, now := newDocClient(t)
	_, err := c.DocumentCreate(context.Background(), Caller{UserID: "u_alice"}, DocumentCreateInput{
		Type: document.TypeContract, Scope: document.ScopeSystem, Name: "plan",
		ResolvedProjectID: "proj_alpha", Now: now,
	})
	require.ErrorIs(t, err, ErrDocumentSystemRejectsProject)
}

func TestDocumentCreate_RequiresCallerIdentity(t *testing.T) {
	c, wsID, now := newDocClient(t)
	_, err := c.DocumentCreate(context.Background(), Caller{}, DocumentCreateInput{
		Type: document.TypeAgent, Scope: document.ScopeProject, Name: "alpha_agent",
		WorkspaceID: wsID, ResolvedProjectID: "proj_alpha", Now: now,
	})
	require.ErrorIs(t, err, ErrDocumentNoCallerIdentity)
}

func TestDocumentCreate_StructuredInvalidJSONRejected(t *testing.T) {
	c, wsID, now := newDocClient(t)
	_, err := c.DocumentCreate(context.Background(), Caller{UserID: "u_alice"}, DocumentCreateInput{
		Type: document.TypeAgent, Scope: document.ScopeProject, Name: "alpha_agent",
		WorkspaceID: wsID, ResolvedProjectID: "proj_alpha", Structured: "{not valid", Now: now,
	})
	require.ErrorIs(t, err, ErrDocumentStructuredInvalid)
}

func TestDocumentUpdate_RejectsImmutableFields(t *testing.T) {
	c, wsID, now := newDocClient(t)
	doc, err := c.DocumentCreate(context.Background(), Caller{UserID: "u_alice"}, DocumentCreateInput{
		Type: document.TypeAgent, Scope: document.ScopeProject, Name: "alpha_agent",
		WorkspaceID: wsID, ResolvedProjectID: "proj_alpha", Now: now,
	})
	require.NoError(t, err)
	body := "new body"
	_, err = c.DocumentUpdate(context.Background(), Caller{UserID: "u_alice"}, DocumentUpdateInput{
		ID:          doc.ID,
		Fields:      document.UpdateFields{Body: &body},
		RawArgs:     map[string]any{"body": "new body", "type": document.TypeContract},
		Memberships: []string{wsID},
		Now:         now,
	})
	require.ErrorIs(t, err, ErrDocumentImmutableField)
}

func TestDocumentUpdate_SystemTierDropsMemberships(t *testing.T) {
	c, _, now := newDocClient(t)
	doc, err := c.DocumentCreate(context.Background(), Caller{UserID: "u_admin"}, DocumentCreateInput{
		Type: document.TypeContract, Scope: document.ScopeSystem, Name: "plan",
		Body: "system contract", Now: now,
	})
	require.NoError(t, err)
	body := "patched body"
	updated, err := c.DocumentUpdate(context.Background(), Caller{UserID: "u_outsider"}, DocumentUpdateInput{
		ID: doc.ID, Fields: document.UpdateFields{Body: &body},
		RawArgs: map[string]any{"body": "patched body"}, Memberships: []string{"wksp_other"}, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, "patched body", updated.Body)
}

func TestDocumentDelete_DefaultsToArchive(t *testing.T) {
	c, wsID, now := newDocClient(t)
	doc, err := c.DocumentCreate(context.Background(), Caller{UserID: "u_alice"}, DocumentCreateInput{
		Type: document.TypeAgent, Scope: document.ScopeProject, Name: "alpha_agent",
		WorkspaceID: wsID, ResolvedProjectID: "proj_alpha", Now: now,
	})
	require.NoError(t, err)
	out, err := c.DocumentDelete(context.Background(), Caller{UserID: "u_alice"}, DocumentDeleteInput{
		ID: doc.ID, Memberships: []string{wsID},
	})
	require.NoError(t, err)
	assert.Equal(t, doc.ID, out.ID)
	assert.Equal(t, string(document.DeleteArchive), out.Mode)
	assert.True(t, out.Deleted)
}

func TestDocumentSearchScoped_StructuredFilterReturnsRows(t *testing.T) {
	c, wsID, now := newDocClient(t)
	_, err := c.deps.Documents.Create(context.Background(), document.Document{
		Type: document.TypeContract, Scope: document.ScopeSystem, Name: "plan", Status: document.StatusActive,
	}, now)
	require.NoError(t, err)
	_, err = c.deps.Documents.Create(context.Background(), document.Document{
		WorkspaceID: wsID, Type: document.TypeAgent, Scope: document.ScopeWorkspace, Name: "alpha_agent", Status: document.StatusActive,
	}, now)
	require.NoError(t, err)
	rows, err := c.DocumentSearchScoped(context.Background(), Caller{UserID: "u_alice"}, DocumentSearchScopedInput{
		Memberships: []string{wsID},
	})
	require.NoError(t, err)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	assert.Contains(t, names, "plan")
	assert.Contains(t, names, "alpha_agent")
}

func TestDocumentList_WalksTierLadder(t *testing.T) {
	c, wsID, now := newDocClient(t)
	// system-tier contract
	_, err := c.deps.Documents.Create(context.Background(), document.Document{
		Type: document.TypeContract, Scope: document.ScopeSystem, Name: "plan", Status: document.StatusActive,
	}, now)
	require.NoError(t, err)
	// workspace-tier agent in alpha
	_, err = c.deps.Documents.Create(context.Background(), document.Document{
		WorkspaceID: wsID, Type: document.TypeAgent, Scope: document.ScopeWorkspace, Name: "alpha_agent", Status: document.StatusActive,
	}, now)
	require.NoError(t, err)

	rows, err := c.DocumentList(context.Background(), Caller{UserID: "u_alice"}, DocumentListInput{
		Options:     document.ListOptions{},
		WorkspaceID: wsID,
		Memberships: []string{wsID},
	})
	require.NoError(t, err)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	assert.Contains(t, names, "plan", "system-tier should be in union")
	assert.Contains(t, names, "alpha_agent", "workspace-tier should be in union")
}

// TestKnownDocumentKinds_ReturnsWrapperFamily asserts the accessor
// enumerates the six wrapper kinds in stable order and returns a
// fresh slice on each call (mutating the result must not leak back
// into wrapperDocumentKinds).
func TestKnownDocumentKinds_ReturnsWrapperFamily(t *testing.T) {
	got := KnownDocumentKinds()
	want := []string{
		document.TypePrinciple,
		document.TypeContract,
		document.TypeSkill,
		document.TypeReviewer,
		document.TypeAgent,
		document.TypeRole,
	}
	assert.Equal(t, want, got)

	got[0] = "tampered"
	again := KnownDocumentKinds()
	assert.Equal(t, want, again, "mutating the returned slice must not affect later calls")
}
