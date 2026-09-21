// Package cli implements the arh command-line frontend (roadmap §18).
//
// The CLI is a thin client of the daemon RPC — no duplicated execution
// logic. `arh daemon start` is the only command that runs the daemon.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/mcp"
	"github.com/xy00099/agent-runtime-harness/internal/rpc"
)

const usage = `arh — Agent Runtime Harness

A local execution and resource broker that lets multiple AI coding agents
safely share heavyweight development tools, devices, caches, licenses and
hardware.

Usage:
  arh <command> [subcommand] [flags]

Daemon:
  arh daemon start            run the runtime daemon (foreground)
  arh daemon start -d         run detached (background)
  arh daemon stop             stop the daemon (graceful session shutdown)
  arh daemon status           show daemon status
  arh daemon endpoint         print the RPC endpoint

Sessions:
  arh session create [--workspace NAME|--dir DIR] [--name NAME]
  arh session list
  arh session inspect ID
  arh session kill ID [--reason R]
  arh session procs ID

Tools:
  arh tool list
  arh tool inspect ID
  arh tool doctor ID
  arh tool resolve "unity >= 6000.0"

Resources & leases:
  arh resource list
  arh lease list [--session ID]
  arh lease acquire --session ID --resource ID [--ttl SEC] [--wait]
  arh lease release --session ID --lease ID

Ports:
  arh port list
  arh port acquire --session ID [--protocol tcp|udp] [--name NAME]

Workspaces:
  arh workspace create --repo PATH --branch BRANCH [--name NAME]
  arh workspace list
  arh workspace remove NAME [--prune]

Execution:
  arh exec TOOL.COMMAND --session ID [--wait] [--timeout-min N]
                        [--arg key=value ...]
                        TOOL.COMMAND examples:
                          my-cmd-tool.run          (registry tool id + command)
                          "unity>=6000.0".test.editmode   (requirement form)
  arh run list [--session ID]
  arh run inspect RUN
  arh logs RUN [--name log.txt|stdout.log|stderr.log|unity.log] [--tail N]
  arh artifacts RUN

MCP:
  arh mcp                     serve the MCP frontend over stdio

Misc:
  arh status                   dashboard: sessions, resources, queues
  arh describe                 policy + environment summary
  arh version

Environment:
  ARH_CONFIG     path to runtime config (default ~/.agent-runtime/config.yaml)
  ARH_STATE_DIR  override runtime.state_dir
`

// Run dispatches the CLI. Returns the process exit code.
func Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "daemon":
		return runDaemon(ctx, rest)
	case "session":
		return runSession(ctx, rest)
	case "tool":
		return runTool(ctx, rest)
	case "resource":
		return runResource(ctx, rest)
	case "lease":
		return runLease(ctx, rest)
	case "port":
		return runPort(ctx, rest)
	case "workspace":
		return runWorkspace(ctx, rest)
	case "exec":
		return runExec(ctx, rest)
	case "run":
		return runRun(ctx, rest)
	case "logs":
		return runLogs(ctx, rest)
	case "artifacts":
		return runArtifacts(ctx, rest)
	case "mcp":
		return runMCP(ctx, rest)
	case "status":
		return runStatus(ctx, rest)
	case "describe":
		return runDescribe(ctx, rest)
	case "version", "--version", "-v":
		fmt.Println("arh " + Version + " (" + buildInfo() + ")")
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// Version is the harness version (overridable at build time with
// -ldflags "-X internal/cli.Version=0.2.0").
var Version = "0.2.0"

func buildInfo() string { return "go" }

// ---------------------------------------------------------------------------
// Config + client helpers
// ---------------------------------------------------------------------------

func loadConfig() *config.Config {
	path := os.Getenv("ARH_CONFIG")
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	if d := os.Getenv("ARH_STATE_DIR"); d != "" {
		cfg.Runtime.StateDir = d
	}
	cfg.Normalize()
	return cfg
}

func dial(ctx context.Context) (*rpc.Client, func()) {
	cfg := loadConfig()
	ep := rpc.DefaultEndpoint(cfg.Runtime.StateDir)
	if cfg.Daemon.Endpoint == "tcp" {
		ep = rpc.Endpoint{Network: "tcp", Address: fmt.Sprintf("%s:%d", cfg.Daemon.Host, cfg.Daemon.Port)}
	}
	c, err := rpc.Dial(ep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arh: %v\n", err)
		os.Exit(1)
	}
	done := func() { _ = c.Close() }
	return c, done
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "arh: %v\n", err)
	return 1
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// newTab starts a tabwriter on stdout.
func newTab() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

func ownerFromEnv() string {
	if o := os.Getenv("ARH_OWNER"); o != "" {
		return o
	}
	return "cli:" + os.Getenv("USERNAME") + os.Getenv("USER")
}

// ---------------------------------------------------------------------------
// Daemon
// ---------------------------------------------------------------------------

func runDaemon(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: arh daemon start|stop|status|endpoint\n")
		return 2
	}
	cfg := loadConfig()
	switch args[0] {
	case "start":
		return daemonStart(ctx, cfg, args[1:])
	case "stop":
		c, done := dial(ctx)
		defer done()
		var out rpc.DescribeResult
		if err := c.Call(ctx, "describe", rpc.Empty{}, &out); err != nil {
			return fail(err)
		}
		fmt.Println("daemon stopped")
		return 0
	case "status":
		c, done := dial(ctx)
		defer done()
		var out rpc.DescribeResult
		if err := c.Call(ctx, "describe", rpc.Empty{}, &out); err != nil {
			return fail(err)
		}
		fmt.Println("daemon: running")
		fmt.Println("endpoint:", endpointString(cfg))
		return 0
	case "endpoint":
		fmt.Println(endpointString(cfg))
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown daemon subcommand %q\n", args[0])
	return 2
}

func endpointString(cfg *config.Config) string {
	if cfg.Daemon.Endpoint == "tcp" {
		return fmt.Sprintf("tcp %s:%d", cfg.Daemon.Host, cfg.Daemon.Port)
	}
	ep := rpc.DefaultEndpoint(cfg.Runtime.StateDir)
	return ep.Network + " " + ep.Address
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func runSession(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: arh session create|list|inspect|kill|procs\n")
		return 2
	}
	c, done := dial(ctx)
	defer done()
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		fs := flag.NewFlagSet("session create", flag.ContinueOnError)
		name := fs.String("name", "", "friendly name")
		ws := fs.String("workspace", "", "workspace name")
		dir := fs.String("dir", "", "explicit workspace dir")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		var res rpc.CreateSessionResult
		err := c.Call(ctx, "session.create", rpc.CreateSessionParams{
			Owner: ownerFromEnv(), Name: *name, Workspace: *ws, WorkspaceDir: *dir,
		}, &res)
		if err != nil {
			return fail(err)
		}
		s := res.Session
		fmt.Printf("session %s created\n  state:    %s\n  home:     %s\n  temp:     %s\n  workspace:%s\n",
			s.ID, s.State, s.HomeDir, s.TempDir, orDash(s.WorkspaceDir))
		return 0
	case "list":
		var out rpc.ListResult[rpc.SessionView]
		if err := c.Call(ctx, "session.list", rpc.Empty{}, &out); err != nil {
			return fail(err)
		}
		w := newTab()
		fmt.Fprintln(w, "SESSION\tNAME\tSTATE\tWORKSPACE\tPROCS\tCREATED")
		for _, s := range out.Items {
			var procs rpc.SessionProcsResult
			_ = c.Call(ctx, "session.procs", rpc.IDParams{ID: s.ID}, &procs)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
				s.ID, orDash(s.Name), s.State, orDash(s.Workspace), len(procs.Items), s.CreatedAt)
		}
		w.Flush()
		return 0
	case "inspect":
		if len(rest) < 1 {
			fmt.Fprint(os.Stderr, "usage: arh session inspect ID\n")
			return 2
		}
		var s rpc.SessionView
		if err := c.Call(ctx, "session.get", rpc.IDParams{ID: rest[0]}, &s); err != nil {
			return fail(err)
		}
		var procs rpc.SessionProcsResult
		_ = c.Call(ctx, "session.procs", rpc.IDParams{ID: s.ID}, &procs)
		printJSON(map[string]any{"session": s, "processes": procs.Items})
		return 0
	case "kill":
		fs := flag.NewFlagSet("session kill", flag.ContinueOnError)
		reason := fs.String("reason", "", "why")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() < 1 {
			fmt.Fprint(os.Stderr, "usage: arh session kill ID\n")
			return 2
		}
		var s rpc.SessionView
		if err := c.Call(ctx, "session.terminate", rpc.IDParams{ID: fs.Arg(0), Reason: *reason}, &s); err != nil {
			return fail(err)
		}
		fmt.Printf("session %s %s\n", s.ID, s.State)
		return 0
	case "procs":
		if len(rest) < 1 {
			fmt.Fprint(os.Stderr, "usage: arh session procs ID\n")
			return 2
		}
		var procs rpc.SessionProcsResult
		if err := c.Call(ctx, "session.procs", rpc.IDParams{ID: rest[0]}, &procs); err != nil {
			return fail(err)
		}
		w := newTab()
		fmt.Fprintln(w, "PID\tLABEL\tEXE\tSTARTED")
		for _, p := range procs.Items {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", p.PID, p.Label, p.Exe, p.StartedAt)
		}
		w.Flush()
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown session subcommand %q\n", sub)
	return 2
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

func runTool(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: arh tool list|inspect|doctor|resolve\n")
		return 2
	}
	c, done := dial(ctx)
	defer done()
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		var out rpc.ListResult[rpc.ToolView]
		if err := c.Call(ctx, "tool.list", rpc.Empty{}, &out); err != nil {
			return fail(err)
		}
		w := newTab()
		fmt.Fprintln(w, "TOOL\tTYPE\tVERSION\tSOURCE\tEXECUTABLE")
		for _, t := range out.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Type, orDash(t.Version), t.Source, t.Executable)
		}
		w.Flush()
		return 0
	case "inspect":
		var id string
		id, rest = peelFirstPositional(rest)
		if id == "" {
			return usageErr("tool inspect ID")
		}
		rest = append([]string{id}, rest...)
		var t rpc.ToolView
		if err := c.Call(ctx, "tool.inspect", rpc.IDParams{ID: rest[0]}, &t); err != nil {
			return fail(err)
		}
		printJSON(t)
		return 0
	case "doctor":
		if len(rest) < 1 {
			return usageErr("tool doctor ID")
		}
		var d rpc.DoctorView
		if err := c.Call(ctx, "tool.doctor", rpc.IDParams{ID: rest[0]}, &d); err != nil {
			return fail(err)
		}
		status := "FAIL"
		if d.OK {
			status = "OK"
		}
		fmt.Printf("tool %s: %s\n  executable: %s\n", d.ToolID, status, d.Executable)
		for _, p := range d.Problems {
			fmt.Printf("  problem: %s\n", p)
		}
		if d.VersionOut != "" {
			fmt.Printf("  version: %s\n", strings.TrimSpace(d.VersionOut))
		}
		return 0
	case "resolve":
		if len(rest) < 1 {
			return usageErr(`tool resolve "unity >= 6000.0"`)
		}
		var t rpc.ToolView
		if err := c.Call(ctx, "tool.resolve", struct {
			Requirement string `json:"requirement"`
		}{Requirement: rest[0]}, &t); err != nil {
			return fail(err)
		}
		fmt.Printf("%s -> %s (%s) %s\n", rest[0], t.ID, t.Version, t.Executable)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown tool subcommand %q\n", sub)
	return 2
}

func usageErr(u string) int {
	fmt.Fprintf(os.Stderr, "usage: arh %s\n", u)
	return 2
}

// ---------------------------------------------------------------------------
// Resources / leases / ports
// ---------------------------------------------------------------------------

func runResource(ctx context.Context, args []string) int {
	c, done := dial(ctx)
	defer done()
	var out rpc.ListResult[rpc.ResourceView]
	if err := c.Call(ctx, "resource.list", rpc.Empty{}, &out); err != nil {
		return fail(err)
	}
	w := newTab()
	fmt.Fprintln(w, "RESOURCE\tMODE\tCAPACITY\tACTIVE\tWAITING")
	for _, r := range out.Items {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\n", r.ID, r.Mode, r.Capacity, r.Active, r.Waiting)
	}
	w.Flush()
	return 0
}

func runLease(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: arh lease list|acquire|release\n")
		return 2
	}
	c, done := dial(ctx)
	defer done()
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("lease list", flag.ContinueOnError)
		session := fs.String("session", "", "filter by session")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		var out rpc.ListResult[rpc.LeaseView]
		err := c.Call(ctx, "lease.list", struct {
			SessionID string `json:"session_id"`
		}{SessionID: *session}, &out)
		if err != nil {
			return fail(err)
		}
		w := newTab()
		fmt.Fprintln(w, "LEASE\tRESOURCE\tSESSION\tSTATE\tCREATED\tEXPIRES")
		for _, l := range out.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", l.ID, l.ResourceID, l.SessionID, l.State, l.CreatedAt, orDash(l.ExpiresAt))
		}
		w.Flush()
		return 0
	case "acquire":
		fs := flag.NewFlagSet("lease acquire", flag.ContinueOnError)
		session := fs.String("session", "", "owning session")
		resource := fs.String("resource", "", "resource id")
		ttl := fs.Int("ttl", 0, "seconds until expiry (0 = none)")
		wait := fs.Bool("wait", false, "queue until available")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *session == "" || *resource == "" {
			return usageErr("lease acquire --session ID --resource ID")
		}
		var l rpc.LeaseView
		callCtx := ctx
		if !*wait {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
		}
		err := c.Call(callCtx, "lease.acquire", rpc.AcquireLeaseParams{
			Owner: ownerFromEnv(), SessionID: *session, ResourceID: *resource,
			TTLSeconds: *ttl, Wait: *wait,
		}, &l)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("lease %s on %s -> session %s (expires %s)\n", l.ID, l.ResourceID, l.SessionID, orDash(l.ExpiresAt))
		return 0
	case "release":
		fs := flag.NewFlagSet("lease release", flag.ContinueOnError)
		session := fs.String("session", "", "owning session")
		lease := fs.String("lease", "", "lease id")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *session == "" || *lease == "" {
			return usageErr("lease release --session ID --lease ID")
		}
		if err := c.Call(ctx, "lease.release", struct {
			Owner     string `json:"owner"`
			SessionID string `json:"session_id"`
			LeaseID   string `json:"lease_id"`
		}{Owner: ownerFromEnv(), SessionID: *session, LeaseID: *lease}, nil); err != nil {
			return fail(err)
		}
		fmt.Println("lease released")
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown lease subcommand %q\n", sub)
	return 2
}

func runPort(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: arh port list|acquire\n")
		return 2
	}
	c, done := dial(ctx)
	defer done()
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		var out rpc.ListResult[rpc.PortView]
		if err := c.Call(ctx, "port.list", rpc.Empty{}, &out); err != nil {
			return fail(err)
		}
		w := newTab()
		fmt.Fprintln(w, "PORT\tPROTOCOL\tSESSION\tNAME")
		for _, p := range out.Items {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", p.Port, p.Protocol, p.SessionID, orDash(p.Name))
		}
		w.Flush()
		return 0
	case "acquire":
		fs := flag.NewFlagSet("port acquire", flag.ContinueOnError)
		session := fs.String("session", "", "owning session")
		protocol := fs.String("protocol", "tcp", "tcp|udp")
		name := fs.String("name", "", "label")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *session == "" {
			return usageErr("port acquire --session ID")
		}
		var p rpc.PortView
		err := c.Call(ctx, "port.allocate", rpc.AllocatePortParams{
			Owner: ownerFromEnv(), SessionID: *session, Protocol: *protocol, Name: *name,
		}, &p)
		if err != nil {
			return fail(err)
		}
		envName := "AGENT_RUNTIME_PORT_" + strings.ToUpper(strings.ReplaceAll(orDefault(p.Name, "PORT"), "-", "_"))
		fmt.Printf("port %d (%s) -> session %s\nexport %s=%d\n", p.Port, p.Protocol, p.SessionID, envName, p.Port)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown port subcommand %q\n", sub)
	return 2
}

func orDefault(s, def string) string { return s }

// ---------------------------------------------------------------------------
// Workspaces
// ---------------------------------------------------------------------------

func runWorkspace(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: arh workspace create|list|remove\n")
		return 2
	}
	c, done := dial(ctx)
	defer done()
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		fs := flag.NewFlagSet("workspace create", flag.ContinueOnError)
		repo := fs.String("repo", "", "path to an existing git repo")
		branch := fs.String("branch", "", "branch for the worktree")
		name := fs.String("name", "", "workspace name (defaults to branch basename)")
		detach := fs.Bool("detach", false, "detached worktree")
		path := fs.String("path", "", "explicit worktree location")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *repo == "" {
			return usageErr("workspace create --repo PATH --branch BRANCH")
		}
		var w rpc.WorkspaceView
		err := c.Call(ctx, "workspace.create", rpc.CreateWorkspaceParams{
			Owner: ownerFromEnv(), Repo: *repo, Branch: *branch, Name: *name,
			Detach: *detach, Path: *path,
		}, &w)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("workspace %s created\n  path:   %s\n  branch: %s\n", w.Name, w.Path, orDash(w.Branch))
		return 0
	case "list":
		var out rpc.ListResult[rpc.WorkspaceView]
		if err := c.Call(ctx, "workspace.list", rpc.Empty{}, &out); err != nil {
			return fail(err)
		}
		w := newTab()
		fmt.Fprintln(w, "WORKSPACE\tBRANCH\tPATH\tLAST SESSION")
		for _, ws := range out.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ws.Name, orDash(ws.Branch), ws.Path, "")
		}
		w.Flush()
		return 0
	case "remove":
		name, rest := peelFirstPositional(rest)
		fs := flag.NewFlagSet("workspace remove", flag.ContinueOnError)
		prune := fs.Bool("prune", false, "also delete the worktree from disk")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if name == "" {
			return usageErr("workspace remove NAME [--prune]")
		}
		err := c.Call(ctx, "workspace.remove", struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Prune bool   `json:"prune"`
		}{Owner: ownerFromEnv(), Name: name, Prune: *prune}, nil)
		if err != nil {
			return fail(err)
		}
		fmt.Println("workspace removed")
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown workspace subcommand %q\n", sub)
	return 2
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// parseToolCommand splits "tool.command" or "\"req\".command".
// Returns toolID, requirement, command.
func parseToolCommand(s string) (toolID, requirement, command string, err error) {
	// Requirement form: "unity >= 6000.0".test.editmode — quotes included.
	if strings.HasPrefix(s, "\"") {
		end := strings.Index(s[1:], "\"")
		if end < 0 {
			return "", "", "", fmt.Errorf("unbalanced quote in %q", s)
		}
		requirement = s[1 : 1+end]
		rest := s[1+end+1:]
		rest = strings.TrimPrefix(rest, ".")
		return "", requirement, rest, nil
	}
	i := strings.Index(s, ".")
	if i <= 0 {
		return "", "", "", fmt.Errorf("tool command must be TOOL.COMMAND (got %q)", s)
	}
	return s[:i], "", s[i+1:], nil
}

// repeatedFlag collects a repeatable string flag.
type repeatedFlag []string

func (r *repeatedFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

// mergeArgs folds repeated --arg k=v entries into the positional map.
// Repeated keys append to a []any (word-split), matching adapter args.
func mergeArgs(pos map[string]any, rep repeatedFlag) map[string]any {
	for _, kv := range rep {
		if i := strings.Index(kv, "="); i > 0 {
			key, val := kv[:i], kv[i+1:]
			words := strings.Fields(val)
			switch existing := pos[key].(type) {
			case []any:
				list := existing
				for _, w := range words {
					list = append(list, w)
				}
				pos[key] = list
			case string:
				list := []any{existing}
				for _, w := range words {
					list = append(list, w)
				}
				pos[key] = list
			default:
				if len(words) > 1 {
					list := make([]any, 0, len(words))
					for _, w := range words {
						list = append(list, w)
					}
					pos[key] = list
				} else {
					pos[key] = val
				}
			}
		}
	}
	return pos
}

// parseKV turns ["key=value with spaces", ...] into adapter args.
// Values are split on spaces (the common case: a command line), producing
// []any of words; quotes are honored via strings.Fields.
func parseKV(args []string) map[string]any {
	m := map[string]any{}
	for _, a := range args {
		if i := strings.Index(a, "="); i > 0 {
			key, val := a[:i], a[i+1:]
			if fields := strings.Fields(val); len(fields) > 1 {
				list := make([]any, len(fields))
				for j, f := range fields {
					list[j] = f
				}
				m[key] = list
			} else {
				m[key] = val
			}
		}
	}
	return m
}

func runExec(ctx context.Context, args []string) int {
	// Go's flag package stops at the first positional argument, so the
	// TOOL.COMMAND must be peeled off before parsing flags.
	var positional []string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional = append(positional, args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	session := fs.String("session", "", "session id")
	wait := fs.Bool("wait", true, "wait for completion (default true; --wait=false fires and forgets)")
	timeout := fs.Int("timeout-min", 0, "timeout minutes (policy default otherwise)")
	flagArgs := repeatedFlag{}
	fs.Var(&flagArgs, "arg", "adapter argument, e.g. --arg args=/c --arg args=echo hi (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := append(positional, fs.Args()...)
	if len(rest) < 1 || *session == "" {
		fmt.Fprint(os.Stderr, "usage: arh exec TOOL.COMMAND --session ID [--wait] [--timeout-min N] [-- k=v ...]\n")
		return 2
	}
	toolID, requirement, command, err := parseToolCommand(rest[0])
	if err != nil {
		return fail(err)
	}
	c, done := dial(ctx)
	defer done()
	var run rpc.RunView
	err = c.Call(ctx, "run.execute", rpc.ExecuteParams{
		Owner:          ownerFromEnv(),
		SessionID:      *session,
		ToolID:         toolID,
		Requirement:    requirement,
		Command:        command,
		Args:           mergeArgs(parseKV(rest[1:]), flagArgs),
		TimeoutMinutes: *timeout,
		Wait:           *wait,
	}, &run)
	if err != nil {
		return fail(err)
	}
	if *wait {
		fmt.Printf("run %s: %s (exit ", run.ID, run.Status)
		if run.ExitCode != nil {
			fmt.Printf("%d", *run.ExitCode)
		} else {
			fmt.Print("-")
		}
		fmt.Printf(", %dms)\n", run.DurationMS)
		if run.Error != "" {
			fmt.Printf("  note: %s\n", run.Error)
		}
	} else {
		fmt.Printf("run %s started (session %s)\n", run.ID, run.SessionID)
	}
	if *wait && run.Status != "SUCCEEDED" {
		return 3
	}
	return 0
}

func runRun(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "usage: arh run list|inspect\n")
		return 2
	}
	c, done := dial(ctx)
	defer done()
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("run list", flag.ContinueOnError)
		session := fs.String("session", "", "filter by session")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		var out rpc.ListResult[rpc.RunView]
		err := c.Call(ctx, "run.list", struct {
			SessionID string `json:"session_id"`
		}{SessionID: *session}, &out)
		if err != nil {
			return fail(err)
		}
		w := newTab()
		fmt.Fprintln(w, "RUN\tSESSION\tTOOL\tCOMMAND\tSTATUS\tEXIT\tDURATION")
		for _, r := range out.Items {
			exit := "-"
			if r.ExitCode != nil {
				exit = fmt.Sprint(*r.ExitCode)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%dms\n", r.ID, r.SessionID, r.ToolID, r.Command, r.Status, exit, r.DurationMS)
		}
		w.Flush()
		return 0
	case "inspect":
		if len(rest) < 1 {
			return usageErr("run inspect RUN")
		}
		var r rpc.RunView
		if err := c.Call(ctx, "run.get", rpc.IDParams{ID: rest[0]}, &r); err != nil {
			return fail(err)
		}
		printJSON(r)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown run subcommand %q\n", sub)
	return 2
}

// peelFirstPositional splits a CLI invocation into (firstPositional, rest)
// before flag parsing: Go's flag package stops at the first non-flag
// argument, so subcommands like `arh logs RUN --name stdout.log` would
// otherwise silently ignore the flags.
func peelFirstPositional(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

// runLogs reads a run log.
func runLogs(ctx context.Context, args []string) int {
	runID, args := peelFirstPositional(args)
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	name := fs.String("name", "log.txt", "log.txt|stdout.log|stderr.log|unity.log")
	tail := fs.Int("tail", 200, "last N lines")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" {
		return usageErr("logs RUN [--name NAME] [--tail N]")
	}
	c, done := dial(ctx)
	defer done()
	var out rpc.LogResult
	err := c.Call(ctx, "run.logs", rpc.LogParams{RunID: runID, Name: *name, Tail: *tail}, &out)
	if err != nil {
		return fail(err)
	}
	if out.Content == "" {
		fmt.Printf("(no %s output)\n", *name)
		return 0
	}
	fmt.Print(out.Content)
	if !strings.HasSuffix(out.Content, "\n") {
		fmt.Println()
	}
	return 0
}

func runArtifacts(ctx context.Context, args []string) int {
	runID, _ := peelFirstPositional(args)
	if runID == "" {
		return usageErr("artifacts RUN")
	}
	c, done := dial(ctx)
	defer done()
	var out rpc.ArtifactsResult
	if err := c.Call(ctx, "run.artifacts", rpc.IDParams{ID: runID}, &out); err != nil {
		return fail(err)
	}
	if len(out.Files) == 0 {
		fmt.Println("(no artifacts)")
		return 0
	}
	for _, f := range out.Files {
		fmt.Println(f)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Status & describe
// ---------------------------------------------------------------------------

func runStatus(ctx context.Context, args []string) int {
	c, done := dial(ctx)
	defer done()
	var sessions rpc.ListResult[rpc.SessionView]
	if err := c.Call(ctx, "session.list", rpc.Empty{}, &sessions); err != nil {
		return fail(err)
	}
	var resources rpc.ListResult[rpc.ResourceView]
	if err := c.Call(ctx, "resource.list", rpc.Empty{}, &resources); err != nil {
		return fail(err)
	}
	fmt.Printf("SESSION      STATE      WORKSPACE        PROCS\n")
	for _, s := range sessions.Items {
		var procs rpc.SessionProcsResult
		_ = c.Call(ctx, "session.procs", rpc.IDParams{ID: s.ID}, &procs)
		fmt.Printf("%-12s %-10s %-16s %d\n", s.ID, s.State, orDash(s.Workspace), len(procs.Items))
	}
	fmt.Println()
	fmt.Printf("RESOURCE             MODE       ACTIVE/WAITING\n")
	for _, r := range resources.Items {
		fmt.Printf("%-20s %-10s %d/%d\n", r.ID, r.Mode, r.Active, r.Waiting)
	}
	_ = sort.Strings
	return 0
}

func runDescribe(ctx context.Context, args []string) int {
	c, done := dial(ctx)
	defer done()
	var out rpc.DescribeResult
	if err := c.Call(ctx, "describe", rpc.Empty{}, &out); err != nil {
		return fail(err)
	}
	fmt.Println("# policy")
	fmt.Print(out.Policy)
	fmt.Println("# environment")
	fmt.Print(out.Env)
	return 0
}

// ---------------------------------------------------------------------------
// MCP
// ---------------------------------------------------------------------------

// runMCP serves the MCP frontend over stdio, dialing the daemon first.
func runMCP(ctx context.Context, args []string) int {
	cfg := loadConfig()
	ep := rpc.DefaultEndpoint(cfg.Runtime.StateDir)
	if cfg.Daemon.Endpoint == "tcp" {
		ep = rpc.Endpoint{Network: "tcp", Address: fmt.Sprintf("%s:%d", cfg.Daemon.Host, cfg.Daemon.Port)}
	}
	client, err := rpc.Dial(ep)
	if err != nil {
		return fail(err)
	}
	defer client.Close()
	srv := mcp.New(client, "mcp")
	return func() int {
		if err := srv.Serve(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "arh mcp: %v\n", err)
			return 1
		}
		return 0
	}()
}
