// Regression for sty_af303c26 (focused slice) — the project page must
// expose the data-project-id host attribute that the realtime bridge
// reads to scope incoming WS events, plus the WSConfig bootstrap that
// head.html turns into window.SATELLITES_WS. Without either, the
// realtime bridge degrades silently and panels stay stale.
//
// sty_a03449d1 added WS-driven task-row patching; sty_7667c9bc moved
// the WS subscription out of storyPanel into the shared
// pages/static/realtime_bridge.js module. The storyPanel now subscribes
// to satellites:realtime:<entity> CustomEvents dispatched by the
// bridge; the per-panel `new SatellitesWS(` site and the
// `_attachRealtimeBridge` helper are gone.
package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/codeindex"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/repo"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// newPortalFullStack mints a Portal wired against the workspace store
// (needed so buildWSConfig can resolve a non-empty WorkspaceID and the
// {{if .WSConfig.WorkspaceID}} head.html guard opens to emit the
// realtime bootstrap). Returns the dependencies the bridge tests need
// to seed an authenticated request.
func newPortalFullStack(t *testing.T, cfg *config.Config) (*Portal, *auth.MemoryUserStore, *auth.MemorySessionStore, *project.MemoryStore, *workspace.MemoryStore) {
	t.Helper()
	users := auth.NewMemoryUserStore()
	sessions := auth.NewMemorySessionStore()
	projects := project.NewMemoryStore()
	ledgerStore := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(ledgerStore)
	docs := document.NewMemoryStore()
	tasks := task.NewMemoryStore()
	repos := repo.NewMemoryStore()
	ws := workspace.NewMemoryStore()
	p, err := New(cfg, satarbor.New("info"), sessions, users, projects, ledgerStore, stories, tasks, docs, repos, codeindex.NewStub(), ws, time.Now())
	if err != nil {
		t.Fatalf("portal.New: %v", err)
	}
	return p, users, sessions, projects, ws
}

func TestProjectDetail_RealtimeBridgeWiring(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, projects, _, _, _, workspaces := newTestPortalWithContracts(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	now := time.Now().UTC()
	if workspaces != nil {
		ws, _ := workspaces.Create(t.Context(), user.ID, "alpha", now)
		_ = workspaces.AddMember(t.Context(), ws.ID, user.ID, "admin", user.ID, now)
	}
	proj, _ := projects.Create(t.Context(), user.ID, "", "alpha-1", now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	// data-project-id is what the realtime bridge reads to scope events.
	if !strings.Contains(body, `data-project-id="`+proj.ID+`"`) {
		t.Errorf("project page missing data-project-id host attribute (sty_af303c26 bridge cannot scope without it)")
	}
}

// sty_a03449d1: task-row patching keeps using data-task-id. sty_7667c9bc
// moved the dispatch into the shared bridge; the storyPanel now sees
// task events as `satellites:realtime:task` CustomEvents and the
// _applyTaskEvent patcher still selects rows by data-task-id.
// sty_08fc8d20: _applyTaskEvent's prefix-strip guard
// `indexOf('task.') !== 0` was replaced by a TASK_STATUS_KINDS
// whitelist Set check (sibling of sty_ec83484f's STORY_STATUS_KINDS
// fix at _applyStoryEvent).
func TestRealtimeBridge_ConsumesTaskEvents(t *testing.T) {
	t.Parallel()
	source := readCommonJS(t)

	// sty_7667c9bc — the shared realtime_bridge dispatches CustomEvents.
	// The panel listens for `satellites:realtime:task` and routes into
	// _applyTaskEvent.
	if !strings.Contains(source, `addEventListener('satellites:realtime:task'`) {
		t.Errorf("storyPanel missing satellites:realtime:task listener; task transitions will not patch in place")
	}
	// sty_08fc8d20 — _applyTaskEvent must gate on the TASK_STATUS_KINDS
	// whitelist, not a prefix-strip. Carries sty_ec83484f's discipline
	// to the task arm.
	if !strings.Contains(source, `TASK_STATUS_KINDS.has(ev.Kind)`) {
		t.Errorf("_applyTaskEvent missing TASK_STATUS_KINDS whitelist check (sty_08fc8d20); a non-status task.* event would corrupt the status pill")
	}
	// The prior prefix-strip predicate inside _applyTaskEvent must be
	// gone; the substring derivation that COMPUTES the status from the
	// Kind stays (the whitelist already guarantees the prefix shape).
	if strings.Contains(source, `ev.Kind.indexOf('task.') !== 0`) {
		t.Errorf("_applyTaskEvent still uses ev.Kind.indexOf('task.') !== 0 guard; sty_08fc8d20 replaced this with TASK_STATUS_KINDS.has(ev.Kind)")
	}
	if !strings.Contains(source, `_applyTaskEvent`) {
		t.Errorf("bridge missing _applyTaskEvent handler; task rows will not patch in place")
	}
	if !strings.Contains(source, `data-task-id`) {
		t.Errorf("task patcher must select rows by data-task-id; ci_id is retired")
	}

	// sty_7667c9bc — the storyPanel-owned WS connection is gone. The
	// realtime_bridge owns the single SatellitesWS per page.
	if strings.Contains(source, `_attachRealtimeBridge`) {
		t.Errorf("storyPanel still defines _attachRealtimeBridge; sty_7667c9bc moves the bridge to realtime_bridge.js")
	}
	if strings.Contains(source, `new window.SatellitesWS(`) || strings.Contains(source, `new SatellitesWS(`) {
		t.Errorf("storyPanel still constructs SatellitesWS directly; sty_7667c9bc moves the WS owner to realtime_bridge.js")
	}

	// The retired contract_instance arm must NOT be reachable —
	// otherwise stale events would still patch nothing useful.
	if strings.Contains(source, `_applyContractEvent`) {
		t.Errorf("bridge still defines _applyContractEvent; the handler is replaced by _applyTaskEvent")
	}
	if strings.Contains(source, `_appendContractRow`) {
		t.Errorf("bridge still defines _appendContractRow; replaced by _appendTaskRow")
	}
}

// sty_7667c9bc — head.html must emit the realtime route table inside
// the existing WSConfig guard so the shared bridge can resolve
// kind → entity without a runtime fetch. The route table has seven
// entries today (story, task, ledger, document, contract, repo,
// project); the test asserts the prefix + entity pairs are present.
func TestProjectDetail_RealtimeRoutesEmbedded(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, projects, workspaces := newPortalFullStack(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	now := time.Now().UTC()
	ws, _ := workspaces.Create(t.Context(), user.ID, "alpha", now)
	proj, _ := projects.Create(t.Context(), user.ID, ws.ID, "alpha-1", now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, `window.SATELLITES_REALTIME_ROUTES`) {
		t.Fatalf("project page missing window.SATELLITES_REALTIME_ROUTES bootstrap; the bridge cannot route events")
	}
	if !strings.Contains(body, `<script src="/static/realtime_bridge.js`) {
		t.Errorf("project page does not load realtime_bridge.js")
	}
	// The realtime mount point is in nav.html — every authenticated
	// page gets it.
	if !strings.Contains(body, `x-data="realtimeBridge"`) {
		t.Errorf("project page missing the realtime bridge Alpine mount point")
	}
	// Each kind_prefix / entity pair must appear in the embedded JSON.
	wantPairs := []struct{ prefix, entity string }{
		{"story.", "story"},
		{"task.", "task"},
		{"ledger.", "ledger"},
		{"document.", "document"},
		{"contract.", "contract"},
		{"repo.", "repo"},
		{"project.", "project"},
	}
	for _, w := range wantPairs {
		needle := `"kind_prefix":"` + w.prefix + `","entity":"` + w.entity + `"`
		if !strings.Contains(body, needle) {
			t.Errorf("project page missing realtime route entry %q in embedded JSON", needle)
		}
	}
}

// TestRealtimeBridge_TaskStatusKindsWhitelist (sty_08fc8d20) locks
// the membership of the TASK_STATUS_KINDS Set declared in
// pages/static/common.js. Mirrors the precedent test for
// STORY_STATUS_KINDS — every status value participating in the
// whitelist must be a status enum the substrate actually emits,
// and the count must equal internal/task/task.go Status* consts.
func TestRealtimeBridge_TaskStatusKindsWhitelist(t *testing.T) {
	t.Parallel()
	source := readCommonJS(t)

	// Assert the Set declaration is present.
	if !strings.Contains(source, `const TASK_STATUS_KINDS = new Set([`) {
		t.Fatalf("pages/static/common.js missing `const TASK_STATUS_KINDS = new Set([` declaration (sty_08fc8d20)")
	}

	// Membership — sourced from internal/task/task.go Status* consts.
	for _, kind := range []string{
		"'task.planned'",
		"'task.published'",
		"'task.enqueued'",
		"'task.claimed'",
		"'task.in_flight'",
		"'task.closed'",
		"'task.archived'",
	} {
		if !strings.Contains(source, kind) {
			t.Errorf("TASK_STATUS_KINDS missing member %s (sourced from internal/task/task.go)", kind)
		}
	}
}

// sty_7667c9bc — the project_ledger page must also expose
// data-project-id so the shared bridge can scope its events the same
// way it does on /projects/{id}. Without it, the bridge would dispatch
// every workspace event the user is authorised to see and the ledger
// panel would patch foreign-project rows.
func TestProjectLedger_DataProjectIDHostAttribute(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: "dev", DevMode: true}
	p, users, sessions, projects, workspaces := newPortalFullStack(t, cfg)
	mux := http.NewServeMux()
	p.Register(mux)

	user := auth.User{ID: "u_alice", Email: "alice@local"}
	users.Add(user)
	now := time.Now().UTC()
	ws, _ := workspaces.Create(t.Context(), user.ID, "alpha", now)
	proj, _ := projects.Create(t.Context(), user.ID, ws.ID, "alpha-1", now)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/projects/"+proj.ID+"/ledger", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, `data-project-id="`+proj.ID+`"`) {
		t.Errorf("ledger page missing data-project-id host attribute (bridge cannot scope without it)")
	}
}

// TestRealtimeBridge_TaskActivityEventDropsAtDispatcher (sty_08fc8d20)
// asserts the dispatcher source itself drops a non-status task.*
// event. Static-source assertion (cheaper than driving a chromedp
// run): post-sty_7667c9bc the per-entity dispatcher lives inside
// `_applyTaskEvent`'s opening guard (not a central `_applyEvent`
// switch); that guard must whitelist on TASK_STATUS_KINDS and must
// not carry the prior prefix-strip predicate.
func TestRealtimeBridge_TaskActivityEventDropsAtDispatcher(t *testing.T) {
	t.Parallel()
	source := readCommonJS(t)

	// Locate the _applyTaskEvent function body — the post-sty_7667c9bc
	// dispatch point for task.* CustomEvents.
	taskBody := extractJSFunctionBody(t, source, "_applyTaskEvent(")
	if !strings.Contains(taskBody, `TASK_STATUS_KINDS.has(ev.Kind)`) {
		t.Errorf("_applyTaskEvent guard not gated on TASK_STATUS_KINDS (sty_08fc8d20)")
	}
	// The prior prefix-strip guard inside _applyTaskEvent must be
	// fully removed (defence-in-depth assertion — the whitelist is
	// the only allowed predicate).
	if strings.Contains(taskBody, `ev.Kind.indexOf('task.') !== 0`) {
		t.Errorf("_applyTaskEvent still references the indexOf('task.') !== 0 prefix-strip guard; sty_08fc8d20 replaced it with TASK_STATUS_KINDS.has(ev.Kind)")
	}
	// The substring derivation that computes the status from the Kind
	// stays — the whitelist guarantees this path is only reached for
	// task.<status> kinds, so the substring is safe.
	if !strings.Contains(taskBody, `ev.Kind.substring('task.'.length)`) {
		t.Errorf("_applyTaskEvent missing the substring derivation; whitelist guarantees this path is only reached for task.<status> kinds (sty_08fc8d20)")
	}
}
