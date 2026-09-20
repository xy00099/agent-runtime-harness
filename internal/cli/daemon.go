// Package cli: daemon lifecycle.
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/daemon"
	"github.com/xy00099/agent-runtime-harness/internal/rpc"
)

// pidFile lives in the state dir.
func pidFilePath(cfg *config.Config) string {
	return filepath.Join(cfg.Runtime.StateDir, "daemon.pid")
}

func daemonStart(ctx context.Context, cfg *config.Config, args []string) int {
	fs := newFlagSet("daemon start")
	detach := fs.Bool("d", false, "detach and run in the background")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *detach {
		return daemonDetach(cfg)
	}
	return daemonForeground(ctx, cfg)
}

// daemonDetach spawns arh daemon start (foreground) as a detached child.
func daemonDetach(cfg *config.Config) int {
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	cmd := exec.Command(exe, "daemon", "start")
	cmd.Env = append(os.Environ(),
		"ARH_STATE_DIR="+cfg.Runtime.StateDir,
		"ARH_CONFIG="+os.Getenv("ARH_CONFIG"),
	)
	// Detach stdio so the child survives the parent terminal.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("start detached daemon: %w", err))
	}
	// Wait briefly for the socket to appear.
	ep := rpc.DefaultEndpoint(cfg.Runtime.StateDir)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := rpc.Dial(ep)
		if err == nil {
			c.Close()
			fmt.Printf("daemon started (pid %d, endpoint %s %s)\n", cmd.Process.Pid, ep.Network, ep.Address)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "arh: daemon did not become ready in 5s (started pid %d)\n", cmd.Process.Pid)
	return 1
}

// daemonForeground runs the daemon in this process until SIGINT/SIGTERM.
func daemonForeground(ctx context.Context, cfg *config.Config) int {
	svc, err := daemon.New(cfg)
	if err != nil {
		return fail(err)
	}
	if err := svc.Recover(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "arh: recovery: %v (continuing)\n", err)
	}

	// pidfile for status/stop.
	if err := os.MkdirAll(cfg.Runtime.StateDir, 0o755); err == nil {
		_ = os.WriteFile(pidFilePath(cfg), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Graceful shutdown on SIGINT/SIGTERM.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()

	mux := rpc.NewMux()
	rpc.Register(mux, svc)

	ep := rpc.DefaultEndpoint(cfg.Runtime.StateDir)
	if cfg.Daemon.Endpoint == "tcp" {
		ep = rpc.Endpoint{Network: "tcp", Address: fmt.Sprintf("%s:%d", cfg.Daemon.Host, cfg.Daemon.Port)}
	}
	// Lease sweep ticker.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = svc.Leases.Sweep(time.Now())
			}
		}
	}()
	fmt.Printf("arh daemon %s listening on %s %s (state: %s)\n",
		Version, ep.Network, ep.Address, cfg.Runtime.StateDir)
	err = rpc.Serve(ctx, ep, mux, nil)
	// Shutdown every live session before exit (roadmap §9 acceptance).
	fmt.Println("daemon: shutting down sessions...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	svc.Shutdown(shutdownCtx)
	_ = os.Remove(pidFilePath(cfg))
	if err != nil {
		fmt.Fprintf(os.Stderr, "arh: %v\n", err)
		return 1
	}
	fmt.Println("daemon stopped")
	return 0
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}
