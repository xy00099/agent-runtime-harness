// Package env builds per-session isolated environments (roadmap §13).
//
// Per session we materialize:
//
//	HOME, TMPDIR            (unix) / USERPROFILE, TMP, TEMP (windows)
//	XDG_CONFIG_HOME, XDG_CACHE_HOME, XDG_STATE_HOME
//
// from an allowlisted inheritance of the daemon environment. Denylisted
// variables are dropped; Set entries are applied last; redacted variables
// have their values masked in any structured log output.
//
// This is execution-state isolation, NOT a security boundary (roadmap §32):
// a child with arbitrary shell access under the same OS user can escape it.
package env

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xy00099/agent-runtime-harness/internal/config"
)

// Manager builds session environments and materializes their directories.
type Manager struct {
	cfg *config.EnvConfig
}

// New creates the manager.
func New(cfg *config.EnvConfig) *Manager {
	return &Manager{cfg: cfg}
}

// Spec describes a session's isolated environment layout.
type Spec struct {
	HomeDir   string
	TempDir   string
	ConfigDir string // XDG_CONFIG_HOME
	CacheDir  string // XDG_CACHE_HOME
	StateDir  string // XDG_STATE_HOME
	LogDir    string // per-session log dir (also XDG_STATE_HOME/log)
}

// Prepare materializes the per-session directories under runtimeDir.
func (m *Manager) Prepare(runtimeDir string) (*Spec, error) {
	sp := &Spec{
		HomeDir:   filepath.Join(runtimeDir, "home"),
		TempDir:   filepath.Join(runtimeDir, "tmp"),
		ConfigDir: filepath.Join(runtimeDir, "xdg", "config"),
		CacheDir:  filepath.Join(runtimeDir, "xdg", "cache"),
		StateDir:  filepath.Join(runtimeDir, "xdg", "state"),
	}
	sp.LogDir = filepath.Join(sp.StateDir, "log")
	for _, d := range []string{sp.HomeDir, sp.TempDir, sp.ConfigDir, sp.CacheDir, sp.LogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("prepare env dirs: %w", err)
		}
	}
	return sp, nil
}

// matchesAllow reports whether name is inherited per the allowlist.
// Entries may be exact names or glob suffixes like "LC_*".
func matchesAllow(allow []string, name string) bool {
	for _, a := range allow {
		if a == "*" {
			return true
		}
		if !strings.HasSuffix(a, "*") {
			if strings.EqualFold(a, name) {
				return true
			}
			continue
		}
		prefix := strings.TrimSuffix(a, "*")
		if strings.EqualFold(prefix, strings.TrimSuffix(name, "")) && strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

func isDenied(deny []string, name string) bool {
	for _, d := range deny {
		if strings.EqualFold(d, name) || (strings.HasSuffix(d, "*") && strings.HasPrefix(strings.ToLower(name), strings.ToLower(strings.TrimSuffix(d, "*")))) {
			return true
		}
	}
	return false
}

// Build constructs the full child environment for a session.
// base is the daemon environment (os.Environ() of the daemon).
func (m *Manager) Build(base []string, spec *Spec) []string {
	denied := map[string]bool{}
	out := map[string]string{}

	// 1. Allowlisted inheritance.
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name, val := kv[:i], kv[i+1:]
		if isDenied(m.cfg.Deny, name) {
			denied[name] = true
			continue
		}
		if matchesAllow(m.cfg.Inherit, name) {
			out[name] = val
		}
	}

	// 2. Isolated roots. Keep *_ORIG copies of the daemon's values so tools
	//    that need the real user home can be configured explicitly.
	if spec != nil {
		if m.cfg.IsolateHome {
			if v, ok := lookup(base, "HOME"); ok && runtimeIsUnix() {
				out["HOME_ORIG"] = v
			}
			if v, ok := lookup(base, "USERPROFILE"); ok {
				out["USERPROFILE_ORIG"] = v
			}
			if v, ok := lookup(base, "APPDATA"); ok {
				out["APPDATA_ORIG"] = v
			}
			if v, ok := lookup(base, "LOCALAPPDATA"); ok {
				out["LOCALAPPDATA_ORIG"] = v
			}
			out["HOME"] = spec.HomeDir
			out["USERPROFILE"] = spec.HomeDir
			out["APPDATA"] = filepath.Join(spec.HomeDir, "AppData", "Roaming")
			out["LOCALAPPDATA"] = filepath.Join(spec.HomeDir, "AppData", "Local")
		}
		if m.cfg.IsolateTemp {
			if v, ok := lookup(base, "TMP"); ok {
				out["TMP_ORIG"] = v
			}
			if v, ok := lookup(base, "TEMP"); ok {
				out["TEMP_ORIG"] = v
			}
			if v, ok := lookup(base, "TMPDIR"); ok {
				out["TMPDIR_ORIG"] = v
			}
			out["TMP"] = spec.TempDir
			out["TEMP"] = spec.TempDir
			out["TMPDIR"] = spec.TempDir
		}
		if m.cfg.IsolateXDG {
			if v, ok := lookup(base, "XDG_CONFIG_HOME"); ok {
				out["XDG_CONFIG_HOME_ORIG"] = v
			}
			out["XDG_CONFIG_HOME"] = spec.ConfigDir
			out["XDG_CACHE_HOME"] = spec.CacheDir
			out["XDG_STATE_HOME"] = spec.StateDir
		}
	}

	// 3. Static Set entries win last (except they never override deny).
	for k, v := range m.cfg.Set {
		if isDenied(m.cfg.Deny, k) {
			denied[k] = true
			continue
		}
		out[k] = v
	}

	// 4. Drop anything denylisted post-set (paranoia).
	for k := range out {
		if isDenied(m.cfg.Deny, k) {
			delete(out, k)
		}
	}

	names := make([]string, 0, len(out))
	for k := range out {
		names = append(names, k)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, k := range names {
		env = append(env, k+"="+out[k])
	}
	return env
}

func lookup(base []string, name string) (string, bool) {
	for _, kv := range base {
		if strings.HasPrefix(kv, name+"=") {
			return kv[len(name)+1:], true
		}
	}
	return "", false
}

// Redact returns a copy of env pairs with redlisted values masked.
// Used for structured logs and inspect output.
func (m *Manager) Redact(pairs map[string]string) map[string]string {
	out := make(map[string]string, len(pairs))
	for k, v := range pairs {
		if isDenied(m.cfg.Redact, k) || isDenied(m.cfg.Deny, k) {
			out[k] = "***REDACTED***"
			continue
		}
		out[k] = v
	}
	return out
}

// IsolateTemp reports whether tmp isolation is enabled.
func (m *Manager) IsolateTemp() bool { return m.cfg.IsolateTemp }

// Describe renders the effective environment policy for CLI output.
func (m *Manager) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "isolate_home: %v\n", m.cfg.IsolateHome)
	fmt.Fprintf(&b, "isolate_tmp: %v\n", m.cfg.IsolateTemp)
	fmt.Fprintf(&b, "isolate_xdg: %v\n", m.cfg.IsolateXDG)
	fmt.Fprintf(&b, "inherit: %s\n", strings.Join(m.cfg.Inherit, ", "))
	if len(m.cfg.Deny) > 0 {
		fmt.Fprintf(&b, "deny: %s\n", strings.Join(m.cfg.Deny, ", "))
	}
	if len(m.cfg.Set) > 0 {
		keys := make([]string, 0, len(m.cfg.Set))
		for k := range m.cfg.Set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "set: %s\n", strings.Join(keys, ", "))
	}
	return b.String()
}

func runtimeIsUnix() bool { return filepath.Separator == '/' }
