// serve.go — `satellites-client serve` cobra group + the four verbs
// (run / start / stop / status) the operator drives the local daemon
// with. The daemon body lives in internal/clientdaemon (sty_5aa20f1b).

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/clientdaemon"
	"github.com/bobmcallan/satellites/internal/config"
)

// registerServeNoun attaches the `serve` cobra group + its four verbs
// to the satellites-client root.
func registerServeNoun(root *cobra.Command) {
	noun := &cobra.Command{
		Use:   "serve",
		Short: "Serve — local push-enqueue daemon for synchronous task dispatch.",
	}
	noun.AddCommand(
		newServeRunCmd(),
		newServeStartCmd(),
		newServeStopCmd(),
		newServeStatusCmd(),
	)
	root.AddCommand(noun)
}

// addServeFlags attaches the daemon-tuning flags shared by `run` and
// `start`. Flag values that map to clientdaemon defaults are kept
// blank-string / zero so the daemon's defaulting logic stays the
// single source of truth.
func addServeFlags(c *cobra.Command) {
	c.Flags().Int("port", 0, "(reserved) future TCP listener port — unused in v1.")
	c.Flags().Int("parallelism", clientdaemon.DefaultParallelism, "Concurrent dispatch slot cap.")
	c.Flags().String("socket", clientdaemon.DefaultSocketPath(), "Unix domain socket path.")
	c.Flags().String("pidfile", clientdaemon.DefaultPidfilePath(), "Pidfile path.")
	c.Flags().String("state", clientdaemon.DefaultStatePath(), "State.json path.")
	c.Flags().String("logfile", clientdaemon.DefaultLogfilePath(), "Daemon log file (start/run).")
	c.Flags().String("logs-dir", clientdaemon.DefaultLogsDir(), "Per-task subprocess log directory.")
	c.Flags().Duration("drain-timeout", clientdaemon.DefaultDrainTimeout, "Drain deadline before daemon-initiated SIGTERM.")
	c.Flags().Int("max-queue", clientdaemon.DefaultMaxQueue, "Soft cap on the in-memory FIFO before /v1/enqueue 503s.")
	c.Flags().Duration("heartbeat", clientdaemon.DefaultHeartbeat, "Lifecycle heartbeat cadence for daemon-spawned tasks.")
}

func newServeRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground (signals shutdown on SIGTERM/SIGINT).",
		RunE:  runServeRun,
	}
	addServeFlags(c)
	return c
}

func newServeStartCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "start",
		Short: "Daemonise the daemon (double-fork) and write its pid to --pidfile.",
		RunE:  runServeStart,
	}
	addServeFlags(c)
	return c
}

func newServeStopCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "stop",
		Short: "SIGTERM the daemon at --pidfile and wait for drain.",
		RunE:  runServeStop,
	}
	c.Flags().String("pidfile", clientdaemon.DefaultPidfilePath(), "Pidfile path.")
	c.Flags().Duration("grace", 30*time.Second, "Wait this long for the pid to exit before reporting failure.")
	return c
}

func newServeStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Report daemon state (running / stale / stopped) from --pidfile.",
		RunE:  runServeStatus,
	}
	c.Flags().String("pidfile", clientdaemon.DefaultPidfilePath(), "Pidfile path.")
	return c
}

func runServeRun(cmd *cobra.Command, _ []string) error {
	parallelism, _ := cmd.Flags().GetInt("parallelism")
	socketPath, _ := cmd.Flags().GetString("socket")
	pidfilePath, _ := cmd.Flags().GetString("pidfile")
	statePath, _ := cmd.Flags().GetString("state")
	logfilePath, _ := cmd.Flags().GetString("logfile")
	logsDir, _ := cmd.Flags().GetString("logs-dir")
	drainTimeout, _ := cmd.Flags().GetDuration("drain-timeout")
	maxQueue, _ := cmd.Flags().GetInt("max-queue")
	heartbeat, _ := cmd.Flags().GetDuration("heartbeat")

	api, err := ensureRemote()
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("serve run: api client: %w", err))
	}

	logger := resolvedLogger
	if logger == nil {
		level := "info"
		if resolvedClientConfig != nil {
			level = resolvedClientConfig.LogLevel
		}
		logger = satarbor.New(level)
	}

	cfg := config.AgentConfig{
		ClaudeBinaryPath: "claude",
		SpawnMCPURL:      effectiveServer() + "/mcp",
		AuthToken:        resolvedToken,
		ExecuteTimeout:   30 * time.Minute,
		LogLevel:         "info",
	}
	if resolvedClientConfig != nil {
		cfg.RepoPath = resolvedClientConfig.RepoPath
		cfg.BranchTemplate = resolvedClientConfig.BranchTemplate
		cfg.WorktreeRoot = resolvedClientConfig.WorktreeRoot
		cfg.LogLevel = resolvedClientConfig.LogLevel
		cfg.ClientConfigPath = resolvedClientConfig.LoadedTOMLPath()
		if resolvedClientConfig.ExecuteTimeout > 0 {
			cfg.ExecuteTimeout = resolvedClientConfig.ExecuteTimeout
		}
	}

	opts := clientdaemon.Options{
		Parallelism:  parallelism,
		MaxQueue:     maxQueue,
		Heartbeat:    heartbeat,
		DrainTimeout: drainTimeout,
		SocketPath:   socketPath,
		PidfilePath:  pidfilePath,
		StatePath:    statePath,
		LogfilePath:  logfilePath,
		LogsDir:      logsDir,
		AgentConfig:  cfg,
		API:          api,
		Logger:       logger,
		Dispatch:     worker.RunDispatched,
	}
	d, err := clientdaemon.New(opts)
	if err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("serve run: daemon: %w", err))
	}

	// Write the pidfile with the foreground pid so `serve stop`
	// signals the right process. `serve start` overwrites this
	// after the double-fork with the child pid.
	if err := writeClientPidFile(pidfilePath, os.Getpid()); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("serve run: pidfile: %w", err))
	}
	defer os.Remove(pidfilePath)

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := d.Run(ctx); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("serve run: %w", err))
	}
	return nil
}

func runServeStart(cmd *cobra.Command, _ []string) error {
	pidfilePath, _ := cmd.Flags().GetString("pidfile")
	logfilePath, _ := cmd.Flags().GetString("logfile")

	if removed, err := cleanStaleClientPid(pidfilePath); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("serve start: %w", err))
	} else if removed {
		fmt.Fprintf(cmd.OutOrStdout(), "satellites-client serve start: removed stale pidfile %s\n", pidfilePath)
	}
	if pid, err := readClientPidFile(pidfilePath); err == nil {
		return cliexit.Newf(cliexit.Server, "serve start: already running (pid=%d, pidfile=%s)", pid, pidfilePath)
	}

	// Forward every flag the operator explicitly set to the spawned
	// `serve run`. Visit (vs VisitAll) only walks user-set flags —
	// defaults stay implicit, so the child inherits the daemon's
	// canonical defaults from clientdaemon.Default* constants.
	argv := []string{"serve", "run"}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		argv = append(argv, "--"+f.Name, f.Value.String())
	})

	pid, err := daemoniseSelf(argv, logfilePath, cmd.ErrOrStderr())
	if err != nil {
		return cliexit.Wrap(cliexit.Server, err)
	}
	if err := writeClientPidFile(pidfilePath, pid); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("serve start: %w", err))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "satellites-client serve started (pid=%d, pidfile=%s)\n", pid, pidfilePath)
	return nil
}

func runServeStop(cmd *cobra.Command, _ []string) error {
	pidfilePath, _ := cmd.Flags().GetString("pidfile")
	grace, _ := cmd.Flags().GetDuration("grace")

	pid, err := readClientPidFile(pidfilePath)
	if err != nil {
		if err == errPidFileNotFound {
			fmt.Fprintf(cmd.OutOrStdout(), "satellites-client serve stop: not running (no pidfile at %s)\n", pidfilePath)
			return nil
		}
		return cliexit.Wrap(cliexit.Server, err)
	}
	if !clientProcessAlive(pid) {
		_ = os.Remove(pidfilePath)
		fmt.Fprintf(cmd.OutOrStdout(), "satellites-client serve stop: pid=%d not alive; removed stale pidfile\n", pid)
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return cliexit.Wrap(cliexit.Server, fmt.Errorf("serve stop: kill pid=%d: %w", pid, err))
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !clientProcessAlive(pid) {
			_ = os.Remove(pidfilePath)
			fmt.Fprintf(cmd.OutOrStdout(), "satellites-client serve stopped (pid=%d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cliexit.Newf(cliexit.Server, "serve stop: pid=%d still alive after %s; pidfile retained", pid, grace)
}

func runServeStatus(cmd *cobra.Command, _ []string) error {
	pidfilePath, _ := cmd.Flags().GetString("pidfile")

	pid, err := readClientPidFile(pidfilePath)
	if err != nil {
		if err == errPidFileNotFound {
			fmt.Fprintf(cmd.OutOrStdout(), "stopped (no pidfile at %s)\n", pidfilePath)
			return nil
		}
		return cliexit.Wrap(cliexit.Server, err)
	}
	if clientProcessAlive(pid) {
		fmt.Fprintf(cmd.OutOrStdout(), "running (pid=%d, pidfile=%s)\n", pid, pidfilePath)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "stale (pid=%d not alive, pidfile=%s) — run `satellites-client serve stop` to clean up\n", pid, pidfilePath)
	return nil
}

