// Package config loads and validates the runtime YAML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// RuntimeConfig controls the runtime daemon itself.
type RuntimeConfig struct {
	StateDir    string `yaml:"state_dir"`
	MaxSessions int    `yaml:"max_sessions"`
}

// DaemonConfig controls the daemon process and endpoint.
type DaemonConfig struct {
	// Endpoint is "unix" (default) or "tcp". With tcp, Host/Port are used;
	// Port 0 means pick a free loopback port.
	Endpoint string `yaml:"endpoint"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	// StopSessionsOnShutdown gracefully stops every live session when the
	// daemon shuts down. Recommended: true (default).
	StopSessionsOnShutdown *bool `yaml:"stop_sessions_on_shutdown"`
}

// EnvConfig controls environment isolation for sessions.
type EnvConfig struct {
	IsolateHome bool              `yaml:"isolate_home"`
	IsolateTemp bool              `yaml:"isolate_tmp"`
	IsolateXDG  bool              `yaml:"isolate_xdg"`
	Inherit     []string          `yaml:"inherit"` // exact names or globs (LC_*)
	Deny        []string          `yaml:"deny"`    // never passed to children
	Set         map[string]string `yaml:"set"`     // static additions
	Redact      []string          `yaml:"redact"`  // logged values are masked
}

// ToolConfig is a statically configured host tool.
type ToolConfig struct {
	Type           string            `yaml:"type"`
	Version        string            `yaml:"version"`
	Executable     string            `yaml:"executable"`
	VersionCommand []string          `yaml:"version_command"`
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
	Capabilities   []string          `yaml:"capabilities"`
	Disabled       bool              `yaml:"disabled"`
}

// ResourceConfig declares a schedulable resource.
type ResourceConfig struct {
	Mode     string            `yaml:"mode"` // exclusive | shared | capacity
	Capacity int               `yaml:"capacity"`
	Provider string            `yaml:"provider"`
	Meta     map[string]string `yaml:"meta"`
}

// PolicyRule is a single allow/deny rule.
type PolicyRule struct {
	Subject string   `yaml:"subject"` // caller prefix match, "*" = all
	Allow   []string `yaml:"allow"`
	Deny    []string `yaml:"deny"`
}

// PolicyConfig is the policy section.
type PolicyConfig struct {
	MaxProcessesPerSession int          `yaml:"max_processes_per_session"`
	MaxRuntimeMinutes      int          `yaml:"max_runtime_minutes"` // default per-run timeout
	MaxConcurrentRuns      int          `yaml:"max_concurrent_runs"`
	Rules                  []PolicyRule `yaml:"rules"`
}

// UnityConfig is the Unity-specific section (adapter + cache strategy).
type UnityConfig struct {
	// Editors maps a friendly id -> install path. Optional; the adapter can
	// also auto-detect from standard hub locations.
	Editors map[string]string `yaml:"editors"`
	// DefaultVersionConstraint such as ">=6000.0" used when a request does
	// not pin a version.
	DefaultVersion string           `yaml:"default_version"`
	Cache          UnityCacheConfig `yaml:"cache"`
}

// UnityCacheConfig controls shared cache strategy (Phase 8).
type UnityCacheConfig struct {
	// Accelerator endpoint; enabling routes Unity cache requests through it.
	AcceleratorEnabled bool   `yaml:"accelerator_enabled"`
	AcceleratorHost    string `yaml:"accelerator_host"`
	// SharedPackageCache is a read-mostly global download cache (UPM).
	// Per-workspace Library/ directories are NEVER shared.
	SharedPackageCache string `yaml:"shared_package_cache"`
}

// Config is the full runtime configuration.
type Config struct {
	Runtime     RuntimeConfig             `yaml:"runtime"`
	Daemon      DaemonConfig              `yaml:"daemon"`
	Tools       map[string]ToolConfig     `yaml:"tools"`
	Resources   map[string]ResourceConfig `yaml:"resources"`
	Environment EnvConfig                 `yaml:"environment"`
	Policy      PolicyConfig              `yaml:"policy"`
	Unity       *UnityConfig              `yaml:"unity"`
}

// DefaultStateDir returns the default state directory.
func DefaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agent-runtime")
}

// Default returns the built-in default configuration.
func Default() *Config {
	return &Config{
		Runtime: RuntimeConfig{
			StateDir:    DefaultStateDir(),
			MaxSessions: 16,
		},
		Daemon: DaemonConfig{Endpoint: "unix"},
		Environment: EnvConfig{
			IsolateHome: true,
			IsolateTemp: true,
			IsolateXDG:  true,
			Inherit:     defaultInherit(),
			Deny:        []string{"AWS_SECRET_ACCESS_KEY", "PROD_TOKEN"},
		},
		Policy: PolicyConfig{
			MaxProcessesPerSession: 32,
			MaxRuntimeMinutes:      120,
			MaxConcurrentRuns:      8,
		},
		Resources: map[string]ResourceConfig{},
		Tools:     map[string]ToolConfig{},
	}
}

func defaultInherit() []string {
	base := []string{"PATH", "LANG", "LC_*", "TERM", "TERMINFO", "SHELL", "USER", "LOGNAME", "HOME_ORIG"}
	if runtime.GOOS == "windows" {
		base = append(base, "SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "USERPROFILE_ORIG",
			"PROGRAMFILES", "PROGRAMDATA", "WINDIR", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
			"APPDATA_ORIG", "LOCALAPPDATA_ORIG", "TMP_ORIG", "TEMP_ORIG")
	} else {
		base = append(base, "TZ", "XDG_SESSION_TYPE", "DISPLAY", "WAYLAND_DISPLAY")
	}
	return base
}

// Load reads the configuration file at path, merged over defaults.
// A missing file is not an error: defaults are returned.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Normalize applies implied defaults after unmarshal.
func (c *Config) Normalize() {
	if c.Runtime.StateDir == "" {
		c.Runtime.StateDir = DefaultStateDir()
	}
	if c.Runtime.MaxSessions <= 0 {
		c.Runtime.MaxSessions = 16
	}
	if c.Daemon.Endpoint == "" {
		c.Daemon.Endpoint = "unix"
	}
	if c.Daemon.Endpoint == "tcp" && c.Daemon.Host == "" {
		c.Daemon.Host = "127.0.0.1"
	}
	if len(c.Environment.Inherit) == 0 {
		c.Environment.Inherit = defaultInherit()
	}
	if c.Policy.MaxProcessesPerSession <= 0 {
		c.Policy.MaxProcessesPerSession = 32
	}
	if c.Policy.MaxRuntimeMinutes <= 0 {
		c.Policy.MaxRuntimeMinutes = 120
	}
	if c.Policy.MaxConcurrentRuns <= 0 {
		c.Policy.MaxConcurrentRuns = 8
	}
	if c.Resources == nil {
		c.Resources = map[string]ResourceConfig{}
	}
	if c.Tools == nil {
		c.Tools = map[string]ToolConfig{}
	}
	// Default resources so lease demos work out of the box; config wins.
	if _, ok := c.Resources["build-slot"]; !ok {
		c.Resources["build-slot"] = ResourceConfig{Mode: "capacity", Capacity: 2}
	}
	if _, ok := c.Resources["unity-license"]; !ok {
		c.Resources["unity-license"] = ResourceConfig{Mode: "exclusive"}
	}
	if _, ok := c.Resources["gpu"]; !ok {
		c.Resources["gpu"] = ResourceConfig{Mode: "exclusive", Provider: "local-gpu"}
	}
	if _, ok := c.Resources["unity-process-slot"]; !ok {
		c.Resources["unity-process-slot"] = ResourceConfig{Mode: "capacity", Capacity: 2}
	}
	if _, ok := c.Resources["android-emulator-slot"]; !ok {
		c.Resources["android-emulator-slot"] = ResourceConfig{Mode: "capacity", Capacity: 2}
	}
}

// Validate checks the configuration for obvious errors.
func (c *Config) Validate() error {
	for id, r := range c.Resources {
		switch r.Mode {
		case "exclusive", "shared":
		case "capacity":
			if r.Capacity <= 0 {
				return fmt.Errorf("resource %q: capacity mode requires capacity >= 1", id)
			}
		default:
			return fmt.Errorf("resource %q: unknown mode %q (want exclusive|shared|capacity)", id, r.Mode)
		}
	}
	for id, t := range c.Tools {
		if t.Type == "" {
			return fmt.Errorf("tool %q: type is required", id)
		}
		// android tools derive binaries from ANDROID_HOME env; unity
		// tools can auto-detect from the Hub.
		if t.Executable == "" && t.Type != "unity" && t.Type != "android" {
			return fmt.Errorf("tool %q: executable is required", id)
		}
	}
	return nil
}

// SocketPath returns the unix socket path inside the state dir.
func (c *Config) SocketPath() string {
	return filepath.Join(c.Runtime.StateDir, "arh.sock")
}

// StateFilePaths returns well-known state file paths.
func (c *Config) StateFilePaths() (sessions, leases, ports, workspaces string) {
	base := filepath.Join(c.Runtime.StateDir, "state")
	return filepath.Join(base, "sessions.json"),
		filepath.Join(base, "leases.json"),
		filepath.Join(base, "ports.json"),
		filepath.Join(base, "workspaces.json")
}

// RunsDir returns the run artifact root.
func (c *Config) RunsDir() string { return filepath.Join(c.Runtime.StateDir, "runs") }

// SessionsDir returns the per-session runtime dirs root.
func (c *Config) SessionsDir() string { return filepath.Join(c.Runtime.StateDir, "sessions") }

// LogsDir returns the daemon log directory.
func (c *Config) LogsDir() string { return filepath.Join(c.Runtime.StateDir, "logs") }
