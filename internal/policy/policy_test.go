package policy

import (
	"strings"
	"testing"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/model"
)

func testEngine(rules []config.PolicyRule) *Engine {
	cfg := config.Default()
	cfg.Normalize()
	cfg.Policy.Rules = rules
	return New(cfg)
}

func TestDefaultAllow(t *testing.T) {
	e := testEngine(nil)
	for _, cap := range []string{CapSessionCreate, CapExecTool, CapLeaseAcquire, CapPortAllocate} {
		if err := e.Evaluate("cli:alice", cap); err != nil {
			t.Fatalf("default should allow %s: %v", cap, err)
		}
	}
	if err := e.Evaluate("cli:alice", CapDaemonAdmin); err == nil {
		t.Fatal("daemon admin must be denied by default")
	}
}

func TestAlwaysDeny(t *testing.T) {
	e := testEngine(nil)
	for _, cap := range []string{"host_process.kill", "tool.install", "tool.modify", "other_session.modify"} {
		if err := e.Evaluate("mcp", cap); err == nil {
			t.Fatalf("%s must always be denied", cap)
		}
	}
}

func TestExplicitDenyRule(t *testing.T) {
	// Deny entries take precedence over allow entries in the same rule.
	e := testEngine([]config.PolicyRule{{
		Subject: "mcp",
		Allow:   []string{"session.create", "tool.list", "tool.execute"},
		Deny:    []string{"tool.execute"},
	}})
	if err := e.Evaluate("mcp", "session.create"); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if err := e.Evaluate("mcp", CapExecTool); err == nil {
		t.Fatal("explicit deny should beat allow")
	}
	// Another subject is unaffected.
	if err := e.Evaluate("cli:bob", CapExecTool); err != nil {
		t.Fatalf("cli:bob should keep default allow: %v", err)
	}
}

func TestSubjectPrefixMatching(t *testing.T) {
	e := testEngine([]config.PolicyRule{{
		Subject: "mcp-restricted",
		Allow:   []string{"session.list"},
		Deny:    nil,
	}})
	if err := e.Evaluate("mcp-restricted:agent-1", "session.list"); err != nil {
		t.Fatalf("prefix subject should match: %v", err)
	}
	if err := e.Evaluate("mcp-restricted:agent-1", CapExecTool); err == nil {
		t.Fatal("explicit rule without exec allow must deny")
	}
}

func TestExecDirRoots(t *testing.T) {
	e := testEngine(nil)
	sess := &model.Session{
		WorkspaceDir: "/tmp/ws",
		RuntimeDir:   "/tmp/rt",
		TempDir:      "/tmp/tmp",
		HomeDir:      "/tmp/home",
	}
	if err := e.ValidateExecDir(sess, "/tmp/ws/sub"); err != nil {
		t.Fatalf("inside workspace should pass: %v", err)
	}
	if err := e.ValidateExecDir(sess, "/etc"); err == nil {
		t.Fatal("outside roots must be denied")
	}
	if !strings.Contains(e.ValidateExecDir(sess, "/etc").Error(), "outside the session roots") {
		t.Fatal("denial should explain the roots")
	}
	if err := e.ValidateExecDir(sess, ""); err != nil {
		t.Fatalf("empty dir defaults to workspace: %v", err)
	}
}
