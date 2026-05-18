//go:build portalui

package portalui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/repo"
)

// TestCSPStrict exercises one user gesture per Alpine page under the
// tightened CSP that drops 'unsafe-eval' (story_739823eb). Every
// subtest depends on the @alpinejs/csp build evaluating the workaround
// `:class="hiddenWhenX"` bindings so the x-show reactivity bug does not
// surface.
//
// The harness applies the production SecurityHeaders middleware (see
// harness.go), so each subtest runs against the same CSP value pprod
// emits. A regression that re-introduces `'unsafe-eval'` will be caught
// by TestSecurityHeaders_AllPresent + TestPortalCSP_HeaderEmittedByHarness;
// this suite catches the inverse — the CSP grant was dropped but the
// page can no longer toggle visibility because the workaround is wrong
// or incomplete.
func TestCSPStrict(t *testing.T) {
	t.Run("nav_hamburger_toggle", func(t *testing.T) {
		h := StartHarness(t)
		parent, cancel := withTimeout(context.Background(), browserDeadline)
		defer cancel()
		browserCtx, cancelBrowser := newChromedpContext(t, parent)
		defer cancelBrowser()

		if err := installSessionCookie(browserCtx, h); err != nil {
			t.Fatalf("install session cookie: %v", err)
		}

		const dropdownDisplay = `(() => {
			const el = document.querySelector('[data-testid="nav-dropdown"]');
			return el ? getComputedStyle(el).display : 'NOT-FOUND';
		})()`

		var dispOnLoad, dispAfterClick, dispAfterSecondClick string
		if err := chromedp.Run(browserCtx,
			chromedp.Navigate(h.BaseURL+"/"),
			chromedp.WaitVisible(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
			chromedp.Sleep(400*time.Millisecond),
			chromedp.Evaluate(dropdownDisplay, &dispOnLoad),
			jsClick(`button[data-testid="nav-hamburger"]`),
			chromedp.Sleep(200*time.Millisecond),
			chromedp.Evaluate(dropdownDisplay, &dispAfterClick),
			jsClick(`button[data-testid="nav-hamburger"]`),
			chromedp.Sleep(200*time.Millisecond),
			chromedp.Evaluate(dropdownDisplay, &dispAfterSecondClick),
		); err != nil {
			t.Fatalf("nav hamburger toggle: %v", err)
		}

		if dispOnLoad != "none" {
			t.Errorf("dropdown on load = %q, want 'none' (closed by default under csp-strict)", dispOnLoad)
		}
		if dispAfterClick == "none" || dispAfterClick == "NOT-FOUND" {
			t.Errorf("dropdown after click = %q, want non-none (open after toggle)", dispAfterClick)
		}
		if dispAfterSecondClick != "none" {
			t.Errorf("dropdown after second click = %q, want 'none' (closed after second toggle — proves x-show workaround is reactive)", dispAfterSecondClick)
		}
	})

	t.Run("project_detail_section_toggle", func(t *testing.T) {
		h := StartHarness(t)
		parent, cancel := withTimeout(context.Background(), browserDeadline)
		defer cancel()
		browserCtx, cancelBrowser := newChromedpContext(t, parent)
		defer cancelBrowser()

		if err := installSessionCookie(browserCtx, h); err != nil {
			t.Fatalf("install session cookie: %v", err)
		}
		projID, err := firstProjectID(browserCtx, h)
		if err != nil {
			t.Fatalf("locate project: %v", err)
		}

		// The stories panel-body is rendered with sectionToggle's
		// open=true initially; clicking the header toggles to closed
		// and re-clicking restores. Display computed style is the
		// observable for the workaround working.
		const bodyDisplay = `(() => {
			const el = document.querySelector('[data-testid="panel-stories"] .panel-body');
			return el ? getComputedStyle(el).display : 'NOT-FOUND';
		})()`
		const headerSelector = `[data-testid="panel-stories"] .panel-header`

		var dispOnLoad, dispAfterFirstClick, dispAfterSecondClick string
		if err := chromedp.Run(browserCtx,
			chromedp.Navigate(h.BaseURL+"/projects/"+projID),
			chromedp.WaitVisible(`[data-testid="panel-stories"]`, chromedp.ByQuery),
			chromedp.Sleep(400*time.Millisecond),
			chromedp.Evaluate(bodyDisplay, &dispOnLoad),
			jsClick(headerSelector),
			chromedp.Sleep(200*time.Millisecond),
			chromedp.Evaluate(bodyDisplay, &dispAfterFirstClick),
			jsClick(headerSelector),
			chromedp.Sleep(200*time.Millisecond),
			chromedp.Evaluate(bodyDisplay, &dispAfterSecondClick),
		); err != nil {
			t.Fatalf("section toggle: %v", err)
		}

		if dispOnLoad == "none" || dispOnLoad == "NOT-FOUND" {
			t.Errorf("stories panel-body on load = %q, want non-none (open by default)", dispOnLoad)
		}
		if dispAfterFirstClick != "none" {
			t.Errorf("stories panel-body after first click = %q, want 'none' (closed after toggle)", dispAfterFirstClick)
		}
		if dispAfterSecondClick == "none" || dispAfterSecondClick == "NOT-FOUND" {
			t.Errorf("stories panel-body after second click = %q, want non-none (re-opened — proves :class binding is reactive)", dispAfterSecondClick)
		}
	})

	t.Run("tasks_board_drawer_hidden_by_default", func(t *testing.T) {
		h := StartHarness(t)
		parent, cancel := withTimeout(context.Background(), browserDeadline)
		defer cancel()
		browserCtx, cancelBrowser := newChromedpContext(t, parent)
		defer cancelBrowser()

		if err := installSessionCookie(browserCtx, h); err != nil {
			t.Fatalf("install session cookie: %v", err)
		}

		const drawerDisplay = `(() => {
			const el = document.querySelector('[data-testid="task-drawer"]');
			return el ? getComputedStyle(el).display : 'NOT-FOUND';
		})()`

		var dispOnLoad string
		if err := chromedp.Run(browserCtx,
			chromedp.Navigate(h.BaseURL+"/tasks"),
			chromedp.WaitVisible(`[data-testid="column-in-flight"]`, chromedp.ByQuery),
			chromedp.Sleep(400*time.Millisecond),
			chromedp.Evaluate(drawerDisplay, &dispOnLoad),
		); err != nil {
			t.Fatalf("tasks page: %v", err)
		}
		if dispOnLoad != "none" {
			t.Errorf("task-drawer on load = %q, want 'none' (closed by default — proves :class binding evaluates)", dispOnLoad)
		}
	})

	t.Run("ledger_tail_toggle", func(t *testing.T) {
		h := StartHarness(t)

		now := time.Now().UTC()
		ctx := context.Background()
		proj, err := h.Projects.Create(ctx, h.UserID, h.WorkspaceID, "csp-strict-ledger", now)
		if err != nil {
			t.Fatalf("seed project: %v", err)
		}
		_, _ = h.Ledger.Append(ctx, ledger.LedgerEntry{
			ProjectID:  proj.ID,
			Type:       ledger.TypeDecision,
			Tags:       []string{"kind:test-seed"},
			Content:    "seed-row",
			Durability: ledger.DurabilityDurable,
			SourceType: ledger.SourceSystem,
			Status:     ledger.StatusActive,
		}, now)

		parent, cancel := withTimeout(context.Background(), browserDeadline)
		defer cancel()
		browserCtx, cancelBrowser := newChromedpContext(t, parent)
		defer cancelBrowser()

		if err := installFastFlag(browserCtx); err != nil {
			t.Fatalf("install fast flag: %v", err)
		}
		if err := installSessionCookie(browserCtx, h); err != nil {
			t.Fatalf("install session cookie: %v", err)
		}

		// Toggle the `tailing` checkbox; the new-rows pill stays
		// hidden because no pending rows exist, but the tailing
		// state must round-trip via the x-model binding under
		// @alpinejs/csp.
		const tailingChecked = `(() => {
			const el = document.querySelector('[data-testid="tail-toggle"]');
			return el ? el.checked : null;
		})()`

		var beforeToggle, afterToggle bool
		if err := chromedp.Run(browserCtx,
			chromedp.Navigate(h.BaseURL+"/projects/"+proj.ID+"/ledger"),
			chromedp.WaitVisible(`[data-testid="tail-toggle"]`, chromedp.ByQuery),
			chromedp.Sleep(400*time.Millisecond),
			chromedp.Evaluate(tailingChecked, &beforeToggle),
			jsClick(`[data-testid="tail-toggle"]`),
			chromedp.Sleep(200*time.Millisecond),
			chromedp.Evaluate(tailingChecked, &afterToggle),
		); err != nil {
			t.Fatalf("ledger tail toggle: %v", err)
		}
		if beforeToggle == afterToggle {
			t.Errorf("tail-toggle state did not change after click (before=%v, after=%v) — Alpine x-model not reactive under csp-strict", beforeToggle, afterToggle)
		}
	})

	t.Run("documents_tab_class_active", func(t *testing.T) {
		h := StartHarness(t)

		now := time.Now().UTC()
		ctx := context.Background()
		if _, err := h.Documents.Create(ctx, document.Document{
			WorkspaceID: h.WorkspaceID, Type: "contract", Scope: "system",
			Name: "csp-strict-doc", Status: "active", Body: "body",
		}, now); err != nil {
			t.Fatalf("seed document: %v", err)
		}

		parent, cancel := withTimeout(context.Background(), browserDeadline)
		defer cancel()
		browserCtx, cancelBrowser := newChromedpContext(t, parent)
		defer cancelBrowser()

		if err := installSessionCookie(browserCtx, h); err != nil {
			t.Fatalf("install session cookie: %v", err)
		}

		// `tabContractActive` getter on documentsView produces an
		// active-class string when type=contract is selected; the
		// :class binding must resolve under @alpinejs/csp.
		const contractTabClass = `(() => {
			const el = document.querySelector('[data-testid="tab-contract"]');
			return el ? el.className : 'NOT-FOUND';
		})()`

		var className string
		if err := chromedp.Run(browserCtx,
			chromedp.Navigate(h.BaseURL+"/documents?type=contract"),
			chromedp.WaitVisible(`[data-testid="type-tabs"]`, chromedp.ByQuery),
			chromedp.Sleep(400*time.Millisecond),
			chromedp.Evaluate(contractTabClass, &className),
		); err != nil {
			t.Fatalf("documents tab class: %v", err)
		}
		if !strings.Contains(className, "active") {
			t.Errorf("contract tab className=%q, want substring 'active' (Alpine :class getter not evaluating)", className)
		}
	})

	t.Run("repo_view_renders", func(t *testing.T) {
		h := StartHarness(t)

		now := time.Now().UTC()
		ctx := context.Background()
		proj, err := h.Projects.Create(ctx, h.UserID, h.WorkspaceID, "csp-strict-repo", now)
		if err != nil {
			t.Fatalf("seed project: %v", err)
		}
		if _, err := h.Repos.Create(ctx, repo.Repo{
			WorkspaceID:   h.WorkspaceID,
			ProjectID:     proj.ID,
			GitRemote:     "git@example.com:csp-strict/main.git",
			DefaultBranch: "main",
			HeadSHA:       "deadbeef00",
			Status:        repo.StatusActive,
		}, now); err != nil {
			t.Fatalf("seed repo: %v", err)
		}

		parent, cancel := withTimeout(context.Background(), browserDeadline)
		defer cancel()
		browserCtx, cancelBrowser := newChromedpContext(t, parent)
		defer cancelBrowser()

		if err := installSessionCookie(browserCtx, h); err != nil {
			t.Fatalf("install session cookie: %v", err)
		}

		const drawerDisplay = `(() => {
			const el = document.querySelector('[data-testid="symbol-drawer"]');
			return el ? getComputedStyle(el).display : 'NOT-FOUND';
		})()`

		var bodyHTML, drawerDisp string
		if err := chromedp.Run(browserCtx,
			chromedp.Navigate(h.BaseURL+"/repo"),
			chromedp.WaitVisible(`[data-testid="repo-header"]`, chromedp.ByQuery),
			chromedp.Sleep(400*time.Millisecond),
			chromedp.OuterHTML("html", &bodyHTML, chromedp.ByQuery),
			chromedp.Evaluate(drawerDisplay, &drawerDisp),
		); err != nil {
			t.Fatalf("repo view: %v", err)
		}
		if !strings.Contains(bodyHTML, "git@example.com:csp-strict/main.git") {
			t.Errorf("repo header missing seeded remote URL")
		}
		if drawerDisp != "none" {
			t.Errorf("symbol-drawer on load = %q, want 'none' (closed by default — proves :class binding evaluates)", drawerDisp)
		}
	})

}
