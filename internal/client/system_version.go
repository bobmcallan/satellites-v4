package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// SystemVersionInput is the input for SystemVersion. The verb takes
// no caller-supplied parameters today; the struct exists for shape
// parity with the rest of the typed surface.
type SystemVersionInput struct{}

// SystemVersionArtifact is one published binary entry in the
// manifest.json payload emitted by the release workflow.
type SystemVersionArtifact struct {
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Filename    string `json:"filename"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
}

// SystemVersionOutput mirrors the wire payload of system_version.
// `Version`, `Build`, `Commit` are the top-level manifest stamp; the
// per-artifact details ride in Artifacts.
type SystemVersionOutput struct {
	Version   string                  `json:"version"`
	Build     string                  `json:"build"`
	Commit    string                  `json:"commit"`
	Artifacts []SystemVersionArtifact `json:"artifacts"`
	FetchedAt time.Time               `json:"fetched_at"`
}

// systemVersionManifestTTL bounds how long a fetched manifest is
// cached before the next caller pays a fresh HTTP round-trip. Short
// enough that a freshly-cut release surfaces quickly; long enough
// that a busy MCP channel does not hammer GitHub.
const systemVersionManifestTTL = 60 * time.Second

// systemVersionFetchTimeout caps the HTTP fetch so a slow GitHub
// response doesn't stall the calling handler.
const systemVersionFetchTimeout = 10 * time.Second

// systemVersionCache caches the most recent successful manifest
// fetch behind a mutex. Holding the cache at package scope keeps it
// shared across the per-call *client.Client snapshots cli() builds.
var (
	systemVersionCacheMu sync.Mutex
	systemVersionCache   *systemVersionCacheEntry
	systemVersionHTTPDo  = http.DefaultClient.Do
)

type systemVersionCacheEntry struct {
	url       string
	fetchedAt time.Time
	output    SystemVersionOutput
}

// resetSystemVersionCache is exposed for tests that need to start
// from a clean cache state (e.g. the TTL coverage subtest). The
// production code-path never calls this.
func resetSystemVersionCache() {
	systemVersionCacheMu.Lock()
	systemVersionCache = nil
	systemVersionCacheMu.Unlock()
}

// ResetSystemVersionCacheForTest is the cross-package test helper for
// the mcpserver/httpserver test files that need to drop the typed
// surface's manifest cache between cases. Wire-layer tests can reach
// it without exporting the cache itself.
func ResetSystemVersionCacheForTest() {
	resetSystemVersionCache()
}

// SystemVersion fetches the GitHub Release manifest configured under
// config.ManifestURL, decodes it, and returns the typed output. The
// parsed manifest is cached for systemVersionManifestTTL so repeated
// boot-time calls do not hammer GitHub. Cache key is the resolved
// manifest URL — switching SATELLITES_MANIFEST_URL invalidates the
// cached entry on the next call.
func (c *Client) SystemVersion(ctx context.Context, caller Caller, in SystemVersionInput) (SystemVersionOutput, error) {
	url := c.deps.ManifestURL
	now := time.Now().UTC()

	systemVersionCacheMu.Lock()
	if systemVersionCache != nil &&
		systemVersionCache.url == url &&
		now.Sub(systemVersionCache.fetchedAt) < systemVersionManifestTTL {
		out := systemVersionCache.output
		systemVersionCacheMu.Unlock()
		return out, nil
	}
	systemVersionCacheMu.Unlock()

	if url == "" {
		return SystemVersionOutput{}, fmt.Errorf("system_version: manifest_url not configured")
	}
	out, err := fetchSystemVersionManifest(ctx, url, now)
	if err != nil {
		return SystemVersionOutput{}, err
	}
	systemVersionCacheMu.Lock()
	systemVersionCache = &systemVersionCacheEntry{url: url, fetchedAt: now, output: out}
	systemVersionCacheMu.Unlock()
	return out, nil
}

// fetchSystemVersionManifest performs the bounded HTTP GET and JSON
// decode. Split out so tests can drive it directly via the package
// HTTP indirection. Returns a wrapped error on transport failure,
// non-2xx status, or malformed JSON.
func fetchSystemVersionManifest(ctx context.Context, url string, now time.Time) (SystemVersionOutput, error) {
	reqCtx, cancel := context.WithTimeout(ctx, systemVersionFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return SystemVersionOutput{}, fmt.Errorf("system_version: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := systemVersionHTTPDo(req)
	if err != nil {
		return SystemVersionOutput{}, fmt.Errorf("system_version: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SystemVersionOutput{}, fmt.Errorf("system_version: fetch %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return SystemVersionOutput{}, fmt.Errorf("system_version: read body: %w", err)
	}
	var wire struct {
		Version   string                  `json:"version"`
		Build     string                  `json:"build"`
		Commit    string                  `json:"commit"`
		Artifacts []SystemVersionArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return SystemVersionOutput{}, fmt.Errorf("system_version: parse manifest: %w", err)
	}
	if wire.Version == "" {
		return SystemVersionOutput{}, fmt.Errorf("system_version: manifest missing version field")
	}
	return SystemVersionOutput{
		Version:   wire.Version,
		Build:     wire.Build,
		Commit:    wire.Commit,
		Artifacts: wire.Artifacts,
		FetchedAt: now,
	}, nil
}
