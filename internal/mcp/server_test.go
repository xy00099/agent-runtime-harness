package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/daemon"
	"github.com/xy00099/agent-runtime-harness/internal/rpc"
)

// startDaemon spins an in-process daemon on a TCP loopback port and returns
// a connected client. (Integration with the real RPC server.)
func startDaemon(t *testing.T) (*rpc.Client, func()) {
	t.Helper()
	cfg := config.Default()
	cfg.Runtime.StateDir = filepath.Join(t.TempDir(), "arh")
	cfg.Daemon.Endpoint = "tcp"
	cfg.Daemon.Host = "127.0.0.1"
	cfg.Daemon.Port = 0 // pick free
	cfg.Normalize()
	svc, err := daemon.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := rpc.NewMux()
	rpc.Register(mux, svc)
	done := make(chan struct{})
	addrCh := make(chan string, 1)
	go func() {
		_ = rpc.Serve(context.Background(), rpc.Endpoint{Network: "tcp", Address: "127.0.0.1:0"}, mux, func(a string) {
			addrCh <- a
		})
		close(done)
	}()
	addr := <-addrCh
	// The ready callback gives the listener address; dial it.
	ep := rpc.Endpoint{Network: "tcp", Address: addr}
	client, err := rpc.Dial(ep)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		client.Close()
		svc.Shutdown(context.Background())
	}
	return client, cleanup
}

func TestMCPSmokeInitializeAndListTools(t *testing.T) {
	client, cleanup := startDaemon(t)
	defer cleanup()

	srv := New(client, "mcp")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"runtime_list_resources","arguments":{}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- srv.ServeIO(ctx, strings.NewReader(input), &out) }()

	// Wait until at least three newline-delimited responses arrived.
	deadline := time.After(8 * time.Second)
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	for strings.Count(out.String(), "\n") < 3 {
		select {
		case <-deadline:
			t.Fatalf("timed out; output so far: %s", out.String())
		case <-tick.C:
		}
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("want 3 responses, got %d: %s", len(lines), out.String())
	}
	var initResp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("initialize response: %v (%s)", err, lines[0])
	}
	if initResp.Result.ServerInfo.Name != "agent-runtime-harness" {
		t.Fatalf("serverInfo: %+v", initResp.Result)
	}
	var listResp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listResp); err != nil {
		t.Fatalf("tools/list response: %v (%s)", err, lines[1])
	}
	if len(listResp.Result.Tools) == 0 {
		t.Fatal("tools/list returned no tools")
	}
	var callResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &callResp); err != nil {
		t.Fatalf("tools/call response: %v (%s)", err, lines[2])
	}
	if callResp.Result.IsError {
		t.Fatalf("tools/call errored: %s", callResp.Result.Content)
	}
	if !strings.Contains(callResp.Result.Content[0].Text, "unity-license") {
		t.Fatalf("resource list did not include unity-license: %s", callResp.Result.Content[0].Text)
	}
}

func TestMCPProtocolVersion(t *testing.T) {
	if protocolVersion == "" {
		t.Fatal("protocol version must be declared")
	}
}

func TestMCPToolSurface(t *testing.T) {
	defs := tools()
	want := []string{
		"runtime_create_session", "runtime_close_session", "runtime_list_sessions",
		"runtime_list_tools", "runtime_execute_tool", "runtime_list_resources",
		"runtime_acquire_resource", "runtime_release_resource", "runtime_list_leases",
		"runtime_allocate_port", "runtime_get_logs", "runtime_get_artifacts",
		"unity_run_tests", "unity_build",
	}
	for _, w := range want {
		if _, ok := defs[w]; !ok {
			t.Errorf("missing MCP tool %q", w)
		}
	}
	// Every tool must have a callable mapping.
	for name, d := range defs {
		if d.call == nil {
			t.Errorf("tool %s has no call mapping", name)
		}
		method, _ := d.call(map[string]any{})
		if method == "" {
			t.Errorf("tool %s maps to empty method", name)
		}
	}
}
