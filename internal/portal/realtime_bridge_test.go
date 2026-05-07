// Regression for sty_af303c26 (focused slice) — the project page must
// expose the data-project-id host attribute that the storyPanel Alpine
// factory reads to scope incoming WS events, plus the WSConfig
// bootstrap that head.html turns into window.SATELLITES_WS. Without
// either, the realtime bridge degrades silently and panels stay stale.
//
// sty_a03449d1 added WS-driven task-row patching: the bridge listens
// for task.<status> events scoped to a visible story, patches the
// matching <tr data-task-id=…> in place, and appends a skeleton row
// for fresh tasks minted with prior_task_id (the retry chain). The JS
// source is asserted alongside the SSR markup to keep both ends in
// lockstep.
package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
)

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

	// data-project-id is what the storyPanel factory reads to scope events.
	if !strings.Contains(body, `data-project-id="`+proj.ID+`"`) {
		t.Errorf("project page missing data-project-id host attribute (sty_af303c26 bridge cannot scope without it)")
	}
}

// sty_a03449d1: the realtime bridge consumes task.<status> events
// (not contract_instance.* — those are gone). Assert the bridge
// source carries the new dispatch arm, the patcher selector keys on
// data-task-id, and the contract_instance arm is removed.
func TestRealtimeBridge_ConsumesTaskEvents(t *testing.T) {
	t.Parallel()
	source := readCommonJS(t)

	// The dispatch must route task.* events to a dedicated handler.
	if !strings.Contains(source, `ev.Kind.indexOf('task.') === 0`) {
		t.Errorf("bridge dispatch missing task.* prefix check; portal will drop task transitions")
	}
	if !strings.Contains(source, `_applyTaskEvent`) {
		t.Errorf("bridge missing _applyTaskEvent handler; task rows will not patch in place")
	}
	if !strings.Contains(source, `data-task-id`) {
		t.Errorf("task patcher must select rows by data-task-id; ci_id is retired")
	}

	// The retired contract_instance arm must NOT be reachable —
	// otherwise stale events would still patch nothing useful.
	if strings.Contains(source, `ev.Kind.indexOf('contract_instance.')`) {
		t.Errorf("bridge still dispatches contract_instance.* events; should be removed in sty_a03449d1")
	}
	if strings.Contains(source, `_applyContractEvent`) {
		t.Errorf("bridge still defines _applyContractEvent; the handler is replaced by _applyTaskEvent")
	}
	if strings.Contains(source, `_appendContractRow`) {
		t.Errorf("bridge still defines _appendContractRow; replaced by _appendTaskRow")
	}
}
