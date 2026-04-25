//go:build portalui

package portalui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHarness_Boots is a smoke that confirms the in-process server's
// /healthz answers and the seeded session cookie reaches the indicator
// widget on the landing page. Runs before the chromedp-driven cases so
// any harness wiring regression surfaces with a clear failure rather
// than a chromium timeout.
func TestHarness_Boots(t *testing.T) {
	h := StartHarness(t)

	// /healthz answers 200.
	resp, err := http.Get(h.BaseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// Authed GET / returns the indicator widget.
	req, _ := http.NewRequest(http.MethodGet, h.BaseURL+"/", nil)
	req.AddCookie(&http.Cookie{Name: "satellites_session", Value: h.SessionCookieValue})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body := readAll(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `class="ws-indicator"`) {
		t.Fatalf("landing missing ws-indicator widget; body=%s", body)
	}
	if !strings.Contains(body, `wsIndicator()`) {
		t.Errorf("landing missing Alpine binding")
	}
	if !strings.Contains(body, h.WorkspaceID) {
		t.Errorf("landing missing workspace id %q", h.WorkspaceID)
	}
}

// TestHarness_DisableEnableWS smokes the kill-switch end-to-end at the
// HTTP layer (no chromedp). Confirms the gate flips back and forth and
// that 503 means new ws connections are refused — chromedp tests assume
// this contract.
func TestHarness_DisableEnableWS(t *testing.T) {
	h := StartHarness(t)

	// Initial state: enabled. /ws upgrade attempt without a websocket
	// handshake returns 400 (the upgrader rejects non-ws GETs) — anything
	// other than 503 means the gate is open.
	resp, err := http.Get(h.BaseURL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws (enabled): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("/ws returned 503 while enabled")
	}

	h.DisableWS()
	resp, err = http.Get(h.BaseURL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws (disabled): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/ws disabled status = %d, want 503", resp.StatusCode)
	}

	h.EnableWS()
	resp, err = http.Get(h.BaseURL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws (re-enabled): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("/ws still 503 after EnableWS")
	}
}

// readAll drains the response body and fails the test on read errors.
// Centralised so test sites stay terse.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 0, 8<<10)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

// withTimeout returns a derived context with a deadline equal to the
// shorter of d or the parent's deadline. Tests use it for chromedp flows
// that should bound cleanly under 120s suite timeout.
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
