// Package containers provides a reusable testcontainers-go harness for
// integration tests that need a real satellites stack: a per-test docker
// network with surrealdb (alias "surrealdb") and a satellites server image
// built from docker/Dockerfile wired to it via SATELLITES_DB_DSN.
//
// One call to StartStack replaces the ~40-line inline boot pattern that
// currently appears across tests/integration/*.go.
package containers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// surrealImage is the upstream surrealdb tag this helper boots. Pinned so
// integration tests don't drift when a new image is published.
const surrealImage = "surrealdb/surrealdb:v3.0.0"

// surrealAlias is the docker-network DNS name the satellites server uses
// in its SATELLITES_DB_DSN to reach surreal.
const surrealAlias = "surrealdb"

// defaultStartupTimeout is the per-container wait budget. Image build +
// boot can take a while on a cold daemon.
const defaultStartupTimeout = 120 * time.Second

// Options configures a Stack. All fields are optional; zero values give a
// stack suitable for SSR + /healthz probes.
type Options struct {
	// ServerEnv adds or overrides environment variables on the satellites
	// server container. Helper-set defaults: SATELLITES_PORT=8080,
	// SATELLITES_ENV=dev, SATELLITES_LOG_LEVEL=info,
	// SATELLITES_DEV_MODE=true, SATELLITES_DB_DSN (points at surreal),
	// SATELLITES_DOCS_DIR=/app/docs.
	ServerEnv map[string]string

	// ServerMounts attaches additional host bind mounts to the server.
	// The docs/ mount is added automatically unless SkipDocsMount is set.
	ServerMounts []mount.Mount

	// SkipDocsMount opts out of the default repo-side docs/ → /app/docs
	// read-only bind mount. Only useful when the server doesn't need
	// docs (rare).
	SkipDocsMount bool

	// StartupTimeout overrides the per-container readiness wait
	// (default 120s).
	StartupTimeout time.Duration
}

// Stack is the booted infra. Callers MUST defer Stop to release
// containers + the docker network.
type Stack struct {
	BaseURL     string
	NetworkName string
	Stop        func()
}

// StartStack boots a per-test docker network, a surrealdb container with
// alias "surrealdb", and a satellites server container built from
// docker/Dockerfile wired to it. Returns once GET /healthz returns 200.
//
// Container + network teardown is registered via t.Cleanup AND exposed
// on the returned Stack.Stop for tests that want to tear down early.
func StartStack(t *testing.T, ctx context.Context, opts Options) *Stack {
	t.Helper()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create docker network: %v", err)
	}
	cleanupNet := func() { _ = net.Remove(context.Background()) }
	t.Cleanup(cleanupNet)

	startup := opts.StartupTimeout
	if startup == 0 {
		startup = defaultStartupTimeout
	}

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        surrealImage,
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"start", "--user", "root", "--pass", "root"},
			Networks:     []string{net.Name},
			NetworkAliases: map[string][]string{
				net.Name: {surrealAlias},
			},
			WaitingFor: wait.ForListeningPort("8000/tcp").WithStartupTimeout(startup),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start surrealdb: %v", err)
	}
	cleanupSurreal := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = surreal.Terminate(stopCtx)
	}
	t.Cleanup(cleanupSurreal)

	root := repoRoot(t)
	env := map[string]string{
		"SATELLITES_PORT":      "8080",
		"SATELLITES_ENV":       "dev",
		"SATELLITES_LOG_LEVEL": "info",
		"SATELLITES_DEV_MODE":  "true",
		"SATELLITES_DB_DSN":    fmt.Sprintf("ws://root:root@%s:8000/rpc/satellites/satellites", surrealAlias),
		"SATELLITES_DOCS_DIR":  "/app/docs",
	}
	for k, v := range opts.ServerEnv {
		env[k] = v
	}

	mounts := append([]mount.Mount(nil), opts.ServerMounts...)
	if !opts.SkipDocsMount {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   filepath.Join(root, "docs"),
			Target:   "/app/docs",
			ReadOnly: true,
		})
	}

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    root,
			Dockerfile: "docker/Dockerfile",
			KeepImage:  true,
		},
		ExposedPorts: []string{"8080/tcp"},
		Env:          env,
		Networks:     []string{net.Name},
		WaitingFor: wait.ForHTTP("/healthz").
			WithPort("8080/tcp").
			WithStartupTimeout(startup).
			WithResponseMatcher(func(body io.Reader) bool {
				_, _ = io.Copy(io.Discard, body)
				return true
			}),
	}
	if len(mounts) > 0 {
		m := mounts
		req.HostConfigModifier = func(hc *container.HostConfig) {
			hc.Mounts = append(hc.Mounts, m...)
		}
	}

	server, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start satellites server: %v", err)
	}
	cleanupServer := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Terminate(stopCtx)
	}
	t.Cleanup(cleanupServer)

	host, err := server.Host(ctx)
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	mapped, err := server.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("server port: %v", err)
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, mapped.Port())

	// Belt-and-braces: plain GET /healthz before handing back, matching
	// the existing portal_test pattern.
	hcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if req2, _ := http.NewRequestWithContext(hcCtx, http.MethodGet, baseURL+"/healthz", nil); req2 != nil {
		if resp, err := http.DefaultClient.Do(req2); err == nil {
			resp.Body.Close()
		}
	}

	return &Stack{
		BaseURL:     baseURL,
		NetworkName: net.Name,
		Stop: func() {
			cleanupServer()
			cleanupSurreal()
			cleanupNet()
		},
	}
}

// repoRoot walks up from this file's compile-time path to the satellites
// repo root. tests/common/containers/stack.go → repo root is three
// parents up.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve stack.go path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
}
