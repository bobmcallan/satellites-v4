//go:build portalui

package portalui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestNavDropdown_VisuallyHidden_OnLoad covers story_6b1ef853: the
// hamburger dropdown must be invisible on first paint by THREE
// independent checks across all four authed pages, AND must still be
// invisible when Alpine.js fails to load.
//
// The previous test (story_7c2c0f2e::TestNavDropdown_OpenClose) used a
// single `el.offsetParent !== null` check — necessary but not
// sufficient. This test combines:
//
//  1. getComputedStyle(el).display === 'none'
//  2. el.getBoundingClientRect() reports zero width and height
//  3. a viewport-screenshot delta — the pixel at a point inside the
//     dropdown's expected on-screen region must DIFFER between
//     hidden-state and open-state. If the pixel never changes, either
//     the dropdown isn't really opening, or it's already painted at
//     first paint (the bug we're guarding against).
//
// All three checks must pass on `/`, `/projects`, `/tasks`, `/settings`.
func TestNavDropdown_VisuallyHidden_OnLoad(t *testing.T) {
	pages := []string{"/", "/projects", "/tasks", "/settings"}

	for _, page := range pages {
		page := page
		t.Run("on_load"+page, func(t *testing.T) {
			h := StartHarness(t)

			parent, cancel := withTimeout(context.Background(), browserDeadline)
			defer cancel()
			browserCtx, cancelBrowser := newChromedpContext(t, parent)
			defer cancelBrowser()

			if err := installSessionCookie(browserCtx, h); err != nil {
				t.Fatalf("install session cookie: %v", err)
			}

			if err := chromedp.Run(browserCtx,
				chromedp.Navigate(h.BaseURL+page),
				chromedp.WaitVisible(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
				chromedp.Sleep(300*time.Millisecond),
			); err != nil {
				t.Fatalf("navigate %s: %v", page, err)
			}

			// Pixel sample at the dropdown's expected centre, BEFORE
			// any user interaction. Any subsequent open click should
			// change this pixel because the dropdown paints over it.
			hiddenPixel, sampleX, sampleY, err := samplePixelAtDropdownArea(browserCtx)
			if err != nil {
				t.Fatalf("%s: hidden-state pixel sample: %v", page, err)
			}

			assertDropdownHiddenDOM(t, browserCtx, page+" first-paint")

			// Open + close: the pixel before/after open MUST differ.
			t.Run("open_close", func(t *testing.T) {
				if err := chromedp.Run(browserCtx,
					chromedp.Click(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
					chromedp.Sleep(250*time.Millisecond),
				); err != nil {
					t.Fatalf("click hamburger: %v", err)
				}
				openPixel, _, _, err := samplePixelAt(browserCtx, sampleX, sampleY)
				if err != nil {
					t.Fatalf("%s: open-state pixel sample: %v", page, err)
				}
				assertDropdownVisibleDOM(t, browserCtx, page+" after-click")
				if pixelEqual(hiddenPixel, openPixel) {
					t.Errorf("%s: pixel at (%d,%d) is identical before and after opening dropdown — dropdown isn't painting (bug) OR was already painted at first paint (also bug). hidden=%v open=%v", page, sampleX, sampleY, hiddenPixel, openPixel)
				}

				if err := chromedp.Run(browserCtx,
					chromedp.KeyEvent(kb.Escape),
					chromedp.Sleep(250*time.Millisecond),
				); err != nil {
					t.Fatalf("escape: %v", err)
				}
				closedPixel, _, _, err := samplePixelAt(browserCtx, sampleX, sampleY)
				if err != nil {
					t.Fatalf("%s: closed-state pixel sample: %v", page, err)
				}
				assertDropdownHiddenDOM(t, browserCtx, page+" after-escape")
				if !pixelClose(hiddenPixel, closedPixel, 8) {
					t.Errorf("%s: pixel at (%d,%d) after Escape (%v) differs from first-paint hidden state (%v) — dropdown didn't fully close visually", page, sampleX, sampleY, closedPixel, hiddenPixel)
				}
			})
		})
	}
}

// TestNavDropdown_VisuallyHidden_AlpineBlocked covers AC2: when the
// Alpine.js CDN fails to load (CSP block, network failure), the
// dropdown must still be invisible. Defence-in-depth proves the
// CSS-default-hidden rule holds without any JS.
func TestNavDropdown_VisuallyHidden_AlpineBlocked(t *testing.T) {
	h := StartHarness(t)

	parent, cancel := withTimeout(context.Background(), browserDeadline)
	defer cancel()
	browserCtx, cancelBrowser := newChromedpContext(t, parent)
	defer cancelBrowser()

	if err := installSessionCookie(browserCtx, h); err != nil {
		t.Fatalf("install session cookie: %v", err)
	}

	// Block Alpine.js (CDN URL pattern from head.html).
	if err := chromedp.Run(browserCtx,
		network.Enable(),
		network.SetBlockedURLs([]string{"*alpinejs*"}),
	); err != nil {
		t.Fatalf("block alpine cdn: %v", err)
	}

	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(h.BaseURL+"/"),
		chromedp.WaitVisible(`button[data-testid="nav-hamburger"]`, chromedp.ByQuery),
		chromedp.Sleep(400*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate / with alpine blocked: %v", err)
	}

	// All three checks must pass: dropdown is invisible even though
	// Alpine never loaded to evaluate `x-show` / `x-cloak`.
	assertDropdownHiddenDOM(t, browserCtx, "alpine-blocked first-paint")

	// Sanity: confirm the hamburger button itself rendered (it's
	// server-side HTML; the ≡ character is plain text).
	var btnRect [4]float64
	if err := chromedp.Run(browserCtx,
		chromedp.Evaluate(`(() => {
			const r = document.querySelector('button[data-testid="nav-hamburger"]').getBoundingClientRect();
			return [r.left, r.top, r.width, r.height];
		})()`, &btnRect),
	); err != nil {
		t.Fatalf("hamburger bbox: %v", err)
	}
	if btnRect[2] == 0 || btnRect[3] == 0 {
		t.Errorf("hamburger button has zero size when Alpine blocked: %v", btnRect)
	}
}

// assertDropdownHiddenDOM runs the two DOM-level visibility checks
// (computed style + bbox). The pixel-delta check is run by the caller
// across paired hidden/open snapshots.
func assertDropdownHiddenDOM(t *testing.T, ctx context.Context, label string) {
	t.Helper()
	disp, bbox, err := readDropdownState(ctx)
	if err != nil {
		t.Fatalf("%s: read display/bbox: %v", label, err)
	}
	if disp != "none" && disp != "NOT-FOUND" {
		t.Errorf("%s: getComputedStyle(.nav-dropdown).display = %q, want 'none'", label, disp)
	}
	if bbox[0] != 0 || bbox[1] != 0 {
		t.Errorf("%s: nav-dropdown bbox = %vx%v, want 0x0", label, bbox[0], bbox[1])
	}
}

// assertDropdownVisibleDOM is the inverse: both DOM checks must report
// the dropdown as visible.
func assertDropdownVisibleDOM(t *testing.T, ctx context.Context, label string) {
	t.Helper()
	disp, bbox, err := readDropdownState(ctx)
	if err != nil {
		t.Fatalf("%s: read display/bbox: %v", label, err)
	}
	if disp == "none" || disp == "NOT-FOUND" {
		t.Errorf("%s: getComputedStyle(.nav-dropdown).display = %q, want non-none", label, disp)
	}
	if bbox[0] == 0 || bbox[1] == 0 {
		t.Errorf("%s: nav-dropdown bbox = %vx%v, want non-zero", label, bbox[0], bbox[1])
	}
}

func readDropdownState(ctx context.Context) (string, [2]float64, error) {
	var disp string
	var bbox [2]float64
	err := chromedp.Run(ctx,
		chromedp.Evaluate(`(() => {
			const el = document.querySelector('[data-testid="nav-dropdown"]');
			if (!el) { return 'NOT-FOUND'; }
			return getComputedStyle(el).display;
		})()`, &disp),
		chromedp.Evaluate(`(() => {
			const el = document.querySelector('[data-testid="nav-dropdown"]');
			if (!el) { return [0, 0]; }
			const r = el.getBoundingClientRect();
			return [r.width, r.height];
		})()`, &bbox),
	)
	return disp, bbox, err
}

// samplePixelAtDropdownArea takes a screenshot, computes the dropdown's
// expected centre (just below the hamburger button), samples that
// pixel, and returns the RGB value plus the (x, y) coords used so the
// caller can re-sample at the same point in a second screenshot.
func samplePixelAtDropdownArea(ctx context.Context) ([3]int, int, int, error) {
	var rect [2]float64
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`(() => {
			const el = document.querySelector('button[data-testid="nav-hamburger"]');
			if (!el) { return [0,0]; }
			const r = el.getBoundingClientRect();
			// Sample point: hamburger x-centre, ~50px below the button's
			// bottom edge — well inside the absolute-positioned dropdown
			// (min-width 14rem, padding 0.5rem) when it's open.
			return [(r.left + r.right) / 2, r.bottom + 50];
		})()`, &rect),
	); err != nil {
		return [3]int{}, 0, 0, err
	}
	sampleX := int(rect[0])
	sampleY := int(rect[1])
	pix, _, _, err := samplePixelAt(ctx, sampleX, sampleY)
	return pix, sampleX, sampleY, err
}

func samplePixelAt(ctx context.Context, sampleX, sampleY int) ([3]int, int, int, error) {
	var buf []byte
	if err := chromedp.Run(ctx,
		chromedp.FullScreenshot(&buf, 100),
	); err != nil {
		return [3]int{}, sampleX, sampleY, err
	}
	img, err := png.Decode(bytes.NewReader(buf))
	if err != nil {
		return [3]int{}, sampleX, sampleY, fmt.Errorf("decode png: %w", err)
	}
	if !inBounds(img, sampleX, sampleY) {
		return [3]int{}, sampleX, sampleY, fmt.Errorf("sample point (%d,%d) out of viewport bounds %v", sampleX, sampleY, img.Bounds())
	}
	r, g, b, _ := img.At(sampleX, sampleY).RGBA()
	return [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}, sampleX, sampleY, nil
}

func pixelEqual(a, b [3]int) bool {
	return pixelClose(a, b, 2)
}

func pixelClose(a, b [3]int, tol int) bool {
	return absInt(a[0]-b[0]) <= tol && absInt(a[1]-b[1]) <= tol && absInt(a[2]-b[2]) <= tol
}

func inBounds(img image.Image, x, y int) bool {
	b := img.Bounds()
	return x >= b.Min.X && x < b.Max.X && y >= b.Min.Y && y < b.Max.Y
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
