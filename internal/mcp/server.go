// Package mcp implements the MCP frontend (roadmap §19): a thin, controlled
// capability surface over the daemon RPC. No raw host-shell passthrough —
// every tool maps to a policy-checked daemon method.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/rpc"
)

const protocolVersion = "2025-06-18"

// Server serves MCP over stdio, forwarding to the daemon.
type Server struct {
	client *rpc.Client
	owner  string
}

// New builds the server.
func New(client *rpc.Client, owner string) *Server {
	return &Server{client: client, owner: owner}
}

// mcpError is an MCP JSON-RPC error.
type mcpError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *mcpError) Error() string { return fmt.Sprintf("mcp %d: %s", e.Code, e.Message) }

// Tool describes one exposed tool.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// toolDef is the declarative spec per exposed tool.
type toolDef struct {
	desc   string
	schema map[string]any
	// call maps MCP args to a daemon method + params pair.
	call func(args map[string]any) (method string, params any)
}

// boolArg coerces MCP loose typing.
func boolArg(a map[string]any, k string) bool {
	if v, ok := a[k]; ok {
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return t == "true" || t == "1"
		case float64:
			return t != 0
		}
	}
	return false
}

func strArg(a map[string]any, k string) string {
	if v, ok := a[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func intArg(a map[string]any, k string) int {
	if v, ok := a[k]; ok {
		switch t := v.(type) {
		case float64:
			return int(t)
		case int:
			return t
		}
	}
	return 0
}

func obj() map[string]any { return map[string]any{"type": "object"} }

func reqProps(props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// tools is the exposed MCP surface (roadmap §11 example list, adapted).
func tools() map[string]toolDef {
	t := map[string]toolDef{}
	t["runtime_list_tools"] = toolDef{
		desc:   "List host tools known to the runtime registry.",
		schema: reqProps(map[string]any{}, ""),
		call: func(a map[string]any) (string, any) {
			return "tool.list", rpc.Empty{}
		},
	}
	t["runtime_list_resources"] = toolDef{
		desc:   "List schedulable resources with live usage and queue depth.",
		schema: reqProps(map[string]any{}, ""),
		call:   func(a map[string]any) (string, any) { return "resource.list", rpc.Empty{} },
	}
	t["runtime_create_session"] = toolDef{
		desc: "Create an isolated execution session (own HOME/TMP/XDG, leases, ports).",
		schema: reqProps(map[string]any{
			"workspace":     strProp("workspace name to bind"),
			"workspace_dir": strProp("explicit workspace directory"),
			"name":          strProp("friendly session name"),
		}),
		call: func(a map[string]any) (string, any) {
			return "session.create", rpc.CreateSessionParams{
				Owner: "mcp", Name: strArg(a, "name"),
				Workspace: strArg(a, "workspace"), WorkspaceDir: strArg(a, "workspace_dir"),
			}
		},
	}
	t["runtime_close_session"] = toolDef{
		desc: "Terminate a session: kills its processes, releases leases and ports.",
		schema: reqProps(map[string]any{
			"session": strProp("session id"),
		}, "session"),
		call: func(a map[string]any) (string, any) {
			return "session.terminate", rpc.IDParams{ID: strArg(a, "session"), Reason: "mcp close"}
		},
	}
	t["runtime_list_sessions"] = toolDef{
		desc:   "List execution sessions.",
		schema: reqProps(map[string]any{}, ""),
		call:   func(a map[string]any) (string, any) { return "session.list", rpc.Empty{} },
	}
	t["runtime_acquire_resource"] = toolDef{
		desc: "Acquire a resource lease (exclusive/shared/capacity) for a session.",
		schema: reqProps(map[string]any{
			"session":     strProp("session id"),
			"resource":    strProp("resource id, e.g. gpu, unity-license"),
			"ttl_seconds": intProp("lease TTL in seconds (0 = no expiry)"),
			"wait":        boolProp("queue until available"),
		}, "session", "resource"),
		call: func(a map[string]any) (string, any) {
			return "lease.acquire", rpc.AcquireLeaseParams{
				Owner: "mcp", SessionID: strArg(a, "session"),
				ResourceID: strArg(a, "resource"),
				TTLSeconds: intArg(a, "ttl_seconds"), Wait: boolArg(a, "wait"),
			}
		},
	}
	t["runtime_release_resource"] = toolDef{
		desc: "Release a lease held by a session.",
		schema: reqProps(map[string]any{
			"session": strProp("session id"),
			"lease":   strProp("lease id"),
		}, "session", "lease"),
		call: func(a map[string]any) (string, any) {
			return "lease.release", struct {
				Owner     string `json:"owner"`
				SessionID string `json:"session_id"`
				LeaseID   string `json:"lease_id"`
			}{Owner: "mcp", SessionID: strArg(a, "session"), LeaseID: strArg(a, "lease")}
		},
	}
	t["runtime_list_leases"] = toolDef{
		desc: "List leases (all or by session).",
		schema: reqProps(map[string]any{
			"session": strProp("filter by session id"),
		}),
		call: func(a map[string]any) (string, any) {
			return "lease.list", struct {
				SessionID string `json:"session_id"`
			}{SessionID: strArg(a, "session")}
		},
	}
	t["runtime_allocate_port"] = toolDef{
		desc: "Allocate an exclusive TCP/UDP port for a session.",
		schema: reqProps(map[string]any{
			"session":  strProp("session id"),
			"protocol": strProp("tcp or udp (default tcp)"),
			"name":     strProp("label; becomes AGENT_RUNTIME_PORT_<NAME>"),
		}, "session"),
		call: func(a map[string]any) (string, any) {
			return "port.allocate", rpc.AllocatePortParams{
				Owner: "mcp", SessionID: strArg(a, "session"),
				Protocol: strArg(a, "protocol"), Name: strArg(a, "name"),
			}
		},
	}
	t["runtime_execute_tool"] = toolDef{
		desc: "Execute a registered host tool inside a session (supervised, leased, logged).",
		schema: reqProps(map[string]any{
			"session":         strProp("session id"),
			"tool_id":         strProp("registry tool id, e.g. my-cmd-tool"),
			"requirement":     strProp(`tool requirement, e.g. "unity >= 6000.0" (alternative to tool_id)`),
			"command":         strProp("adapter command, e.g. run, test.editmode, build"),
			"args":            obj(),
			"timeout_minutes": intProp("override per-run timeout"),
			"wait":            boolProp("wait for completion (default true)"),
		}, "session", "command"),
		call: func(a map[string]any) (string, any) {
			return "run.execute", rpc.ExecuteParams{
				Owner: "mcp", SessionID: strArg(a, "session"),
				ToolID: strArg(a, "tool_id"), Requirement: strArg(a, "requirement"),
				Command: strArg(a, "command"), Args: argsMap(a),
				TimeoutMinutes: intArg(a, "timeout_minutes"), Wait: boolArg(a, "wait"),
			}
		},
	}
	t["unity_run_tests"] = toolDef{
		desc: "Run Unity tests (editmode or playmode) in a session workspace.",
		schema: reqProps(map[string]any{
			"session":         strProp("session id"),
			"mode":            strProp("editmode | playmode (default editmode)"),
			"timeout_minutes": intProp("override timeout"),
		}, "session"),
		call: func(a map[string]any) (string, any) {
			mode := strings.ToLower(strArg(a, "mode"))
			if mode == "" {
				mode = "editmode"
			}
			if mode != "editmode" && mode != "playmode" {
				mode = "editmode"
			}
			return "run.execute", rpc.ExecuteParams{
				Owner: "mcp", SessionID: strArg(a, "session"),
				Requirement: "unity", Command: "test." + mode,
				TimeoutMinutes: intArg(a, "timeout_minutes"), Wait: true,
			}
		},
	}
	t["unity_build"] = toolDef{
		desc: "Build a Unity player from a session workspace.",
		schema: reqProps(map[string]any{
			"session":         strProp("session id"),
			"target":          strProp("build target, e.g. Android, iOS, StandaloneWindows64"),
			"method":          strProp("CIScript.BuildPlayer method name"),
			"timeout_minutes": intProp("override timeout"),
		}, "session", "method"),
		call: func(a map[string]any) (string, any) {
			return "run.execute", rpc.ExecuteParams{
				Owner: "mcp", SessionID: strArg(a, "session"),
				Requirement: "unity", Command: "build",
				Args: map[string]any{
					"target": strArg(a, "target"),
					"method": strArg(a, "method"),
				},
				TimeoutMinutes: intArg(a, "timeout_minutes"), Wait: true,
			}
		},
	}
	t["android_list_devices"] = toolDef{
		desc:   "List adb devices (physical + emulators) visible to the runtime.",
		schema: reqProps(map[string]any{}, ""),
		call: func(a map[string]any) (string, any) {
			return "run.execute", rpc.ExecuteParams{Owner: "mcp", ToolID: "android-sdk", Command: "devices", Wait: true}
		},
	}
	t["android_build_apk"] = toolDef{
		desc: "Build a debug APK from a source directory (aapt2 + d8 + zipalign + apksigner).",
		schema: reqProps(map[string]any{
			"session": strProp("session id"),
			"src":     strProp("source dir containing AndroidManifest.xml"),
			"package": strProp("application package id"),
		}, "session", "src"),
		call: func(a map[string]any) (string, any) {
			return "run.execute", rpc.ExecuteParams{
				Owner: "mcp", SessionID: strArg(a, "session"), ToolID: "android-sdk",
				Command: "build.apk",
				Args:    map[string]any{"src": strArg(a, "src"), "package": strArg(a, "package")},
				Wait:    true,
			}
		},
	}
	t["android_run_tests"] = toolDef{
		desc: "Run instrumented tests on a leased device/emulator serial.",
		schema: reqProps(map[string]any{
			"session": strProp("session id"),
			"serial":  strProp("adb serial, e.g. emulator-5554 (leased exclusively)"),
			"cmd":     strProp("shell command, e.g. am instrument -w ..."),
		}, "session", "serial", "cmd"),
		call: func(a map[string]any) (string, any) {
			return "run.execute", rpc.ExecuteParams{
				Owner: "mcp", SessionID: strArg(a, "session"), ToolID: "android-sdk",
				Command: "shell",
				Args: map[string]any{
					"serial": strArg(a, "serial"),
					"cmd":    strArg(a, "cmd"),
				},
				Wait: true,
			}
		},
	}
	t["android_screenshot"] = toolDef{
		desc: "Capture a screenshot from a leased device as a run artifact.",
		schema: reqProps(map[string]any{
			"session": strProp("session id"),
			"serial":  strProp("adb serial (leased exclusively)"),
		}, "session", "serial"),
		call: func(a map[string]any) (string, any) {
			return "run.execute", rpc.ExecuteParams{
				Owner: "mcp", SessionID: strArg(a, "session"), ToolID: "android-sdk",
				Command: "screenshot",
				Args:    map[string]any{"serial": strArg(a, "serial")},
				Wait:    true,
			}
		},
	}
	t["runtime_get_logs"] = toolDef{
		desc: "Read a run's logs (log.txt, stdout.log, stderr.log, unity.log).",
		schema: reqProps(map[string]any{
			"run":  strProp("run id"),
			"name": strProp("which log (default log.txt)"),
			"tail": intProp("last N lines (default 200)"),
		}, "run"),
		call: func(a map[string]any) (string, any) {
			return "run.logs", rpc.LogParams{
				RunID: strArg(a, "run"), Name: strArg(a, "name"), Tail: intArg(a, "tail"),
			}
		},
	}
	t["runtime_get_artifacts"] = toolDef{
		desc: "List collected artifacts of a run (test reports, builds).",
		schema: reqProps(map[string]any{
			"run": strProp("run id"),
		}, "run"),
		call: func(a map[string]any) (string, any) {
			return "run.artifacts", rpc.IDParams{ID: strArg(a, "run")}
		},
	}
	t["runtime_list_runs"] = toolDef{
		desc: "List runs (all or by session).",
		schema: reqProps(map[string]any{
			"session": strProp("filter by session id"),
		}),
		call: func(a map[string]any) (string, any) {
			return "run.list", struct {
				SessionID string `json:"session_id"`
			}{SessionID: strArg(a, "session")}
		},
	}
	t["runtime_get_session"] = toolDef{
		desc: "Inspect one session (state, dirs, processes).",
		schema: reqProps(map[string]any{
			"session": strProp("session id"),
		}, "session"),
		call: func(a map[string]any) (string, any) {
			return "session.get", rpc.IDParams{ID: strArg(a, "session")}
		},
	}
	return t
}

func argsMap(a map[string]any) map[string]any {
	if v, ok := a["args"].(map[string]any); ok {
		return v
	}
	return nil
}

// Serve reads MCP requests from stdin and writes responses to stdout.
func (s *Server) Serve(ctx context.Context) error {
	return s.ServeIO(ctx, os.Stdin, os.Stdout)
}

// ServeIO serves MCP over arbitrary reader/writer (testable).
func (s *Server) ServeIO(ctx context.Context, in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	defs := tools()
	for {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := dec.Decode(&req); err != nil {
			return nil // stdin closed
		}
		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
					"protocolVersion": protocolVersion,
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo": map[string]any{
						"name": "agent-runtime-harness", "version": "0.1.0",
					},
				},
			})
		case "notifications/initialized", "initialized":
			// no response for notifications
		case "ping":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{},
			})
		case "tools/list":
			list := []Tool{}
			for name, d := range defs {
				list = append(list, Tool{Name: name, Description: d.desc, InputSchema: d.schema})
			}
			// stable order
			for i := 1; i < len(list); i++ {
				for j := i; j > 0 && list[j].Name < list[j-1].Name; j-- {
					list[j], list[j-1] = list[j-1], list[j]
				}
			}
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": list},
			})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments,omitempty"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				s.writeErr(enc, req.ID, -32602, "bad tools/call params: %v", err)
				continue
			}
			def, ok := defs[p.Name]
			if !ok {
				s.writeErr(enc, req.ID, -32601, "unknown tool %q", p.Name)
				continue
			}
			var args map[string]any
			if len(p.Arguments) > 0 {
				_ = json.Unmarshal(p.Arguments, &args)
			}
			method, params := def.call(args)
			var result any
			callCtx, cancel := context.WithTimeout(ctx, callTimeoutFor(p.Name))
			err := s.client.Call(callCtx, method, params, &result)
			cancel()
			if err != nil {
				// Tool execution errors are surfaced as tool results with
				// isError=true per MCP spec, not as protocol errors.
				_ = enc.Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{
						"content": []any{map[string]any{
							"type": "text", "text": err.Error(),
						}},
						"isError": true,
					},
				})
				continue
			}
			text, _ := json.MarshalIndent(result, "", "  ")
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []any{map[string]any{
						"type": "text", "text": string(text),
					}},
				},
			})
		default:
			if req.ID != nil {
				s.writeErr(enc, req.ID, -32601, "method %q not found", req.Method)
			}
		}
	}
}

// callTimeoutFor picks generous ceilings per tool (execution may be long).
func callTimeoutFor(name string) time.Duration {
	switch name {
	case "runtime_execute_tool", "unity_run_tests", "unity_build":
		return 6 * time.Hour
	case "runtime_acquire_resource":
		return 10 * time.Minute
	default:
		return 2 * time.Minute
	}
}

func (s *Server) writeErr(enc *json.Encoder, id json.RawMessage, code int, format string, a ...any) {
	_ = enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": &mcpError{Code: code, Message: fmt.Sprintf(format, a...)},
	})
}
