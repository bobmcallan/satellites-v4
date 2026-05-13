package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSystemVersion_HappyPath covers a well-formed manifest reachable
// over HTTP: typed output mirrors the manifest fields and FetchedAt
// stamps the caller's clock.
func TestSystemVersion_HappyPath(t *testing.T) {
	resetSystemVersionCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		    "version":"0.0.269",
		    "build":"2026-05-13-12-00-00",
		    "commit":"abcd1234",
		    "artifacts":[
		      {"os":"linux","arch":"amd64","filename":"satellites-client-linux-amd64","sha256":"deadbeef","download_url":"https://example.invalid/satellites-client-linux-amd64"},
		      {"os":"linux","arch":"arm64","filename":"satellites-client-linux-arm64","sha256":"feedface","download_url":"https://example.invalid/satellites-client-linux-arm64"}
		    ]
		}`))
	}))
	t.Cleanup(srv.Close)

	c := New(Deps{ManifestURL: srv.URL})
	out, err := c.SystemVersion(context.Background(), Caller{UserID: "u_test"}, SystemVersionInput{})
	require.NoError(t, err)
	assert.Equal(t, "0.0.269", out.Version)
	assert.Equal(t, "2026-05-13-12-00-00", out.Build)
	assert.Equal(t, "abcd1234", out.Commit)
	require.Len(t, out.Artifacts, 2)
	assert.Equal(t, "linux", out.Artifacts[0].OS)
	assert.Equal(t, "amd64", out.Artifacts[0].Arch)
	assert.Equal(t, "deadbeef", out.Artifacts[0].SHA256)
	assert.False(t, out.FetchedAt.IsZero())
}

// TestSystemVersion_MalformedJSON returns an error rather than
// surfacing a half-populated SystemVersionOutput.
func TestSystemVersion_MalformedJSON(t *testing.T) {
	resetSystemVersionCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	t.Cleanup(srv.Close)

	c := New(Deps{ManifestURL: srv.URL})
	_, err := c.SystemVersion(context.Background(), Caller{}, SystemVersionInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse manifest")
}

// TestSystemVersion_NetworkError surfaces a wrapped error when the
// HTTP fetch can't complete (e.g. server closed before response).
func TestSystemVersion_NetworkError(t *testing.T) {
	resetSystemVersionCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijacker not supported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	c := New(Deps{ManifestURL: srv.URL})
	_, err := c.SystemVersion(context.Background(), Caller{}, SystemVersionInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "system_version")
}

// TestSystemVersion_TTLCache asserts that the second call within the
// TTL window does not hit the upstream server.
func TestSystemVersion_TTLCache(t *testing.T) {
	resetSystemVersionCache()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"0.0.269","build":"b","commit":"c","artifacts":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := New(Deps{ManifestURL: srv.URL})
	if _, err := c.SystemVersion(context.Background(), Caller{}, SystemVersionInput{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := c.SystemVersion(context.Background(), Caller{}, SystemVersionInput{}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("upstream hit count = %d, want 1 (cache miss)", got)
	}
}

// TestSystemVersion_EmptyManifestURL returns an error rather than
// hitting "" — guards against a deployment that forgot to set the
// config knob.
func TestSystemVersion_EmptyManifestURL(t *testing.T) {
	resetSystemVersionCache()
	c := New(Deps{})
	_, err := c.SystemVersion(context.Background(), Caller{}, SystemVersionInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest_url")
}

// TestSystemVersion_Non200Status surfaces upstream HTTP failures
// (e.g. 503) as typed errors.
func TestSystemVersion_Non200Status(t *testing.T) {
	resetSystemVersionCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := New(Deps{ManifestURL: srv.URL})
	_, err := c.SystemVersion(context.Background(), Caller{}, SystemVersionInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 503")
}

// TestSystemVersion_ContextCancel honours a cancelled caller context.
func TestSystemVersion_ContextCancel(t *testing.T) {
	resetSystemVersionCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	c := New(Deps{ManifestURL: srv.URL})
	_, err := c.SystemVersion(ctx, Caller{}, SystemVersionInput{})
	require.Error(t, err)
	if !strings.Contains(err.Error(), "context") &&
		!strings.Contains(err.Error(), "deadline") &&
		!strings.Contains(err.Error(), "fetch") {
		t.Fatalf("unexpected error: %v", err)
	}
}
