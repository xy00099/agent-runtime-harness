// Package rpc is the daemon transport: JSON-RPC 2.0 over a local stream
// socket. Default endpoint is a unix socket (named pipe on Windows);
// TCP loopback is available for constrained environments.
//
// The wire format is deliberately boring so any language can talk to it.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Request is a JSON-RPC request/notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // nil for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string { return fmt.Sprintf("rpc %d: %s", e.Code, e.Message) }

// Standard codes.
const (
	CodeParse    = -32700
	CodeInvalid  = -32600
	CodeMethod   = -32601
	CodeParams   = -32602
	CodeInternal = -32603
	// Application errors use the -32000..-32099 band.
	CodeApp = -32000
)

// Handler serves one method.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Mux maps method names to handlers.
type Mux struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewMux builds the mux.
func NewMux() *Mux { return &Mux{handlers: map[string]Handler{}} }

// Handle registers a method.
func (m *Mux) Handle(method string, h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[method] = h
}

// dispatch invokes a handler.
func (m *Mux) dispatch(ctx context.Context, method string, params json.RawMessage) (any, error) {
	m.mu.RLock()
	h, ok := m.handlers[method]
	m.mu.RUnlock()
	if !ok {
		return nil, &Error{Code: CodeMethod, Message: fmt.Sprintf("method %q not found", method)}
	}
	return h(ctx, params)
}

// ServeConn handles one connection until EOF.
func (m *Mux) ServeConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 1<<20)
	encoder := json.NewEncoder(conn)
	writeMu := sync.Mutex{}
	for {
		if ctx.Err() != nil {
			return
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return // EOF or broken pipe
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		var req Request
		if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
			_ = encoder.Encode(Response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &Error{Code: CodeParse, Message: err.Error()},
			})
			continue
		}
		if req.Method == "" {
			_ = encoder.Encode(Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &Error{Code: CodeInvalid, Message: "missing method"},
			})
			continue
		}
		result, err := m.dispatch(ctx, req.Method, req.Params)
		if req.ID == nil {
			continue // notification: no response, errors are logged server-side
		}
		resp := Response{JSONRPC: "2.0", ID: req.ID}
		if err != nil {
			var rpcErr *Error
			if errors.As(err, &rpcErr) {
				resp.Error = rpcErr
			} else {
				resp.Error = &Error{Code: CodeInternal, Message: err.Error()}
			}
		} else if result != nil {
			raw, mErr := json.Marshal(result)
			if mErr != nil {
				resp.Error = &Error{Code: CodeInternal, Message: mErr.Error()}
			} else {
				resp.Result = raw
			}
		} else {
			resp.Result = json.RawMessage("null")
		}
		writeMu.Lock()
		_ = encoder.Encode(resp)
		writeMu.Unlock()
	}
}

// Endpoint describes where the daemon listens.
type Endpoint struct {
	Network string // "unix" | "tcp"
	Address string // socket path or host:port
}

// DefaultEndpoint resolves the local socket path. AF_UNIX filesystem
// sockets work on every supported platform (Windows 10 1803+ included), so
// the state dir hosts arh.sock everywhere.
func DefaultEndpoint(stateDir string) Endpoint {
	return Endpoint{Network: "unix", Address: filepath.Join(stateDir, "arh.sock")}
}

// Serve listens on the endpoint and serves connections until ctx ends.
// The ready callback fires with the bound address (port may be dynamic).
func Serve(ctx context.Context, ep Endpoint, mux *Mux, ready func(addr string)) error {
	if err := os.MkdirAll(filepath.Dir(ep.Address), 0o755); ep.Network == "unix" && err != nil {
		return fmt.Errorf("socket dir: %w", err)
	}
	// Remove a stale socket file (previous daemon crashed without cleanup).
	if ep.Network == "unix" {
		if fi, err := os.Lstat(ep.Address); err == nil {
			if fi.Mode()&os.ModeSocket != 0 {
				_ = os.Remove(ep.Address)
			} else {
				// A non-socket file blocks bind; refuse to delete random
				// files, so pick a variant name instead.
				ep.Address = ep.Address + ".1"
			}
		}
	}
	ln, err := net.Listen(ep.Network, ep.Address)
	if err != nil {
		return fmt.Errorf("listen %s %s: %w", ep.Network, ep.Address, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		if ep.Network == "unix" {
			_ = os.Remove(ep.Address)
		}
	}()
	if ready != nil {
		ready(ln.Addr().String())
	}
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			if strings.Contains(err.Error(), "use of closed") {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			mux.ServeConn(ctx, c)
		}(conn)
	}
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client talks to the daemon.
type Client struct {
	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	enc    *json.Encoder
	seq    int
}

// Dial connects to the endpoint with a timeout.
func Dial(ep Endpoint) (*Client, error) {
	conn, err := net.DialTimeout(ep.Network, ep.Address, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s %s: %w (is the daemon running? try `arh daemon start`)", ep.Network, ep.Address, err)
	}
	return &Client{
		conn:   conn,
		reader: bufio.NewReaderSize(conn, 1<<20),
		enc:    json.NewEncoder(conn),
	}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Call invokes a method and decodes the result into out (may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	id := c.seq
	rawID, _ := json.Marshal(id)
	if params == nil {
		params = struct{}{}
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req := Request{JSONRPC: "2.0", ID: rawID, Method: method, Params: rawParams}
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		_ = c.conn.SetDeadline(deadline)
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}
	if err := c.enc.Encode(req); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	// Read responses, skipping notifications (server never sends any today,
	// but be tolerant).
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		var resp Response
		if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if !bytesEqual(resp.ID, rawID) {
			continue
		}
		if resp.Error != nil {
			return &Error{Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	}
}

func bytesEqual(a, b json.RawMessage) bool { return string(a) == string(b) }

// Errorf builds an application error.
func Errorf(format string, a ...any) *Error {
	return &Error{Code: CodeApp, Message: fmt.Sprintf(format, a...)}
}

// ErrorfCode builds an error with a specific code.
func ErrorfCode(code int, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

var _ = io.EOF
