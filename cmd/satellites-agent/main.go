// Command satellites-agent is the satellites worker binary. On boot
// it loads its TOML config, builds an arbor logger, opens an MCP
// transport against the configured endpoint, optionally subscribes
// to the substrate's WS hub for event-driven wakes, and runs the
// worker.Loop until SIGINT or SIGTERM.
//
// The body lives in run() so the lifecycle test in main_test.go can
// drive boot + shutdown directly without forking a subprocess.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args, os.Stdout, os.Stderr))
}

// run is the binary's testable entry point. It parses args, loads
// config, builds the worker pipeline, and blocks on Loop until ctx
// is cancelled (SIGINT/SIGTERM in production, manual cancel in
// tests). Exit code: 0 clean shutdown, 1 fatal error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("satellites-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to TOML config file (overrides SATELLITES_AGENT_CONFIG)")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	cfg, warnings, err := config.LoadAgent(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "satellites-agent: config: %v\n", err)
		return 1
	}

	logger := satarbor.New(cfg.LogLevel)
	logger.Info().
		Str("binary", "satellites-agent").
		Str("version", config.Version).
		Str("build", config.Build).
		Str("commit", config.GitCommit).
		Str("config_path", cfg.LoadedTOMLPath()).
		Str("mcp_url", cfg.MCPURL).
		Str("hub_url", cfg.HubURL).
		Msgf("satellites-agent %s", config.GetFullVersion())

	for _, w := range warnings {
		logger.Warn().Str("warning", w).Msg("agent: startup warning")
	}

	client := worker.NewPlaceholderClient(*cfg, logger)

	// Wake-channel wiring: when hub_url is empty the agent runs in
	// polling-only mode (Worker.Loop's IdleBackoff drives Claim);
	// when set, the WS subscriber emits one WakeEvent per
	// task.published frame.
	var wakeCh <-chan worker.WakeEvent
	if cfg.HubURL != "" && len(subscribeWorkspaces(cfg)) > 0 {
		ws := worker.NewWSClient(worker.WSConfig{
			HubURL:                cfg.HubURL,
			AuthToken:             cfg.AuthToken,
			SubscribeWorkspaceIDs: subscribeWorkspaces(cfg),
			SinceID:               cfg.SubscribeSinceID,
			MinBackoff:            cfg.WSReconnectMinBackoff,
			MaxBackoff:            cfg.WSReconnectMaxBackoff,
		}, logger)
		wakeCh = ws.Run(ctx)
	}

	w := worker.New(client, worker.Config{
		WorkerID:          cfg.WorkerID,
		WorkspaceIDs:      cfg.WorkspaceIDs,
		IdleBackoff:       cfg.IdleBackoff,
		HeartbeatInterval: cfg.HeartbeatInterval,
		ExecuteTimeout:    cfg.ExecuteTimeout,
		WakeChan:          wakeCh,
	}, logger)

	if err := w.Loop(ctx); err != nil && !isExpectedShutdownErr(err) {
		fmt.Fprintf(stderr, "satellites-agent: loop: %v\n", err)
		return 1
	}
	return 0
}

// subscribeWorkspaces returns the workspace ids the WS subscriber
// should open connections to. SubscribeWorkspaceIDs takes precedence;
// when empty the loop falls back to WorkspaceIDs (so the operator
// only configures one list when the same set is used for both
// polling-claim narrowing and WS subscribe).
func subscribeWorkspaces(cfg *config.AgentConfig) []string {
	if len(cfg.SubscribeWorkspaceIDs) > 0 {
		return cfg.SubscribeWorkspaceIDs
	}
	return cfg.WorkspaceIDs
}

// isExpectedShutdownErr reports whether err is the kind of context-
// cancel signal Worker.Loop returns on a graceful SIGTERM. Treating
// it as a clean exit lets run() return 0 from the canonical shutdown
// path.
func isExpectedShutdownErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
