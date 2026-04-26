# tests/portalui — chromedp E2E suite for the WS indicator

This package is the browser-driven follow-up coverage for the portal
connection indicator widget shipped in story_ac3e4057 (10.4). It boots
the satellites server in process using the production constructors
(`portal.New`, `wshandler.New`, `hub.NewAuthHub`) wired against the
package-internal memory stores, then drives a headless Chromium via
`github.com/chromedp/chromedp` to assert the widget's state machine
end-to-end.

## Build tag

Every Go file in this package carries `//go:build portalui`. The default
`go test ./...` run skips the package — chromedp + its transitive deps
do **not** load.

## Running the suite

```
go test -tags=portalui ./tests/portalui/... -timeout=120s
```

If `chromium` (or any compatible Chromium / Chrome binary `chromedp` can
find) is not installed locally, the chromedp-driven tests `t.Skip` with
the underlying error. The two non-chromedp smokes (`TestHarness_Boots`,
`TestHarness_DisableEnableWS`) still run and verify the harness wiring.

## What the tests cover

| Test | Story AC |
|---|---|
| `TestHarness_Boots` | Harness wiring + indicator markup on the landing page |
| `TestHarness_DisableEnableWS` | `/ws` kill-switch contract |
| `TestIndicator_LiveOnConnect` | Sanity: client connects, AuthHub admits, dot turns green |
| `TestIndicator_DropToReconnecting_RecoverGreen` | AC4 — server outage → amber → green |
| `TestIndicator_ProlongedOutage_TurnsRed` | AC5 — prolonged outage → red + retry button |
| `TestIndicator_RetryButton_Recovers` | AC6 — click retry → reconnects |
| `TestIndicator_DebugPanel_RendersEvents` | AC7 — `?debug=true` panel shows live events |
| `TestPortal_AuthedWalk` | story_4fc0d35c — full authed flow (dev-signin → dashboard → workspace menu → /projects → /tasks → hamburger). Includes regression guards for story_690b8f5c (empty workspace menu) and story_0f415ab3 (empty /projects). |
| `TestProjects_DevUserSeesDefaultProject` | story_0f415ab3 — focused per-user default project seed |
| `TestWorkspaceSwitcher_DropdownDoesNotReflowNav` | story_4d1ef14f — workspace switcher height stability |
| `TestDevMode_QuickSignin` | story_7105204f — single-click dev signin |

## Authed flow walk

`TestPortal_AuthedWalk` is the catch-all "real user navigates the
portal" test. When you ship a new authed page, extend it with a step
that asserts both the page renders **and** the empty/broken edge cases
fail loudly. The pattern from the existing assertions:

- Resolve the link/button (`a[href="/X"]`, `[data-testid="..."]`).
- Wait for the next page's anchor element (`section.panel-headed`,
  `footer.footer`, …).
- Assert the expected content count (rows ≥ 1, children ≥ 1, no
  empty-state copy).

## Notable design choices

- **`__SATELLITES_WS_FAST` JS gate.** `pages/static/ws.js` reads
  `window.__SATELLITES_WS_FAST === true` at module load and uses
  compressed backoff timings (50 ms base, 200 ms cap, 30 ms zero-flicker)
  when set. Tests inject the flag via DevTools' `addScriptToEvaluateOnNewDocument`
  before any in-page script runs. The flag is strict-`=== true`; production
  is unaffected by accidental truthy values.
- **Login bypass.** The harness pre-mints a session via
  `auth.SessionStore.Create`; chromedp installs the cookie via
  `network.SetCookie` before navigating. Auth-flow coverage already lives
  in `tests/integration/portal_test.go`.
- **Server-outage simulation.** `Harness.DisableWS()` flips a kill-switch
  middleware (returns 503 to new `/ws` requests) and force-closes any
  tracked `net.Conn` that handled a prior `/ws` request. The browser
  observes the same `onclose` signal it would on a real server crash.

## CI

CI integration is **out of scope** for this story by AC carve-out. A
follow-up infrastructure story should:

1. Install `chromium` (or `google-chrome-stable`) into the build image.
2. Add a CI job stage `make portal-ui` (or the inline `go test -tags=portalui …`)
   gated to run after the default suite passes.
3. Decide artifact-upload policy for failure screenshots / DOM dumps.

Until that story lands, run the suite locally before merging changes
that touch `pages/static/ws.js`, `pages/templates/nav.html`,
`internal/wshandler/`, or `internal/hub/`.
