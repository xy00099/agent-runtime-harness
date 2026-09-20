// Package registry implements the tool registry (roadmap §10): host-installed
// tools as managed resources, with detection, validation, version query and
// requirement resolution ("unity >= 6000.0" -> concrete install).
package registry

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/model"
)

// Registry manages known tools.
type Registry struct {
	mu        sync.RWMutex
	tools     map[string]*model.Tool
	detectors []func() []*model.Tool
}

// New builds a registry from config.
func New(cfg *config.Config) *Registry {
	r := &Registry{tools: map[string]*model.Tool{}}
	for id, tc := range cfg.Tools {
		t := &model.Tool{
			ID:             id,
			Type:           tc.Type,
			Version:        tc.Version,
			Executable:     tc.Executable,
			VersionCommand: tc.VersionCommand,
			Args:           tc.Args,
			Env:            tc.Env,
			Capabilities:   tc.Capabilities,
			Disabled:       tc.Disabled,
			Source:         "config",
		}
		r.tools[id] = t
	}
	// Unity detection runs lazily via Detect; registry ships the detector.
	r.detectors = append(r.detectors, detectUnity(cfg.Unity))
	return r
}

// Register adds or replaces a tool (detection results).
func (r *Registry) Register(t *model.Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.ID] = t
}

// Get returns a tool by id.
func (r *Registry) Get(id string) (*model.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[id]
	return t, ok
}

// List returns all tools sorted by id.
func (r *Registry) List() []*model.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*model.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Detect runs all detectors and registers their findings. Returns the newly
// registered tool ids.
func (r *Registry) Detect() []string {
	var found []string
	for _, det := range r.detectors {
		for _, t := range det() {
			if _, exists := r.Get(t.ID); exists {
				continue // config beats detection
			}
			t.Source = "detected"
			r.Register(t)
			found = append(found, t.ID)
		}
	}
	return found
}

// ---------------------------------------------------------------------------
// Doctor: executable validation and version probing
// ---------------------------------------------------------------------------

// DoctorResult is the outcome of a health check.
type DoctorResult struct {
	ToolID       string    `json:"tool_id"`
	OK           bool      `json:"ok"`
	ExecutableOK bool      `json:"executable_ok"`
	Exists       bool      `json:"exists"`
	Executable   string    `json:"executable"`
	VersionOut   string    `json:"version_output,omitempty"`
	VersionErr   string    `json:"version_error,omitempty"`
	Problems     []string  `json:"problems,omitempty"`
	CheckedAt    time.Time `json:"checked_at"`
}

// Doctor validates a tool: executable resolves, is executable, and
// (optionally) the version command runs.
func (r *Registry) Doctor(ctx context.Context, id string) (*DoctorResult, error) {
	t, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", id)
	}
	res := &DoctorResult{ToolID: id, Executable: t.Executable, CheckedAt: time.Now()}
	problems := func(format string, a ...any) {
		res.Problems = append(res.Problems, fmt.Sprintf(format, a...))
	}
	if t.Executable == "" {
		problems("tool %s (%s) has no executable", id, t.Type)
		res.OK = false
		return res, nil
	}
	exe, err := exec.LookPath(t.Executable)
	if err != nil {
		res.Exists = false
		problems("executable not found on PATH or absolute: %s", t.Executable)
		problems("if this is a macOS .app, use the binary inside Contents/MacOS/")
		res.OK = false
		return res, nil
	}
	res.ExecutableOK = true
	res.Exists = true
	res.Executable = exe
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(exe); err == nil && fi.Mode()&0o111 == 0 {
			problems("executable is not marked executable (chmod +x?)")
			res.ExecutableOK = false
		}
	}
	if len(t.VersionCommand) > 0 {
		cmd := exec.CommandContext(ctx, exe, t.VersionCommand...)
		out, err := cmd.CombinedOutput()
		res.VersionOut = firstN(string(out), 400)
		if err != nil {
			res.VersionErr = err.Error()
			problems("version command failed: %s", strings.TrimSpace(firstN(string(out), 200)))
		}
	}
	res.OK = len(res.Problems) == 0
	return res, nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------------------
// Requirement resolution
// ---------------------------------------------------------------------------

// Requirement is a parsed "type constraint version" triple.
type Requirement struct {
	Type       string
	Constraint string // "", =, >=, >, <, <=, ^
	Version    string
	Raw        string
}

var reqRe = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z0-9_-]*)\s*(>=|<=|>|<|=|\^)?\s*([0-9][A-Za-z0-9._-]*)?\s*$`)

// ParseRequirement parses "unity >= 6000.0", "java = 17", "go".
func ParseRequirement(raw string) (*Requirement, error) {
	m := reqRe.FindStringSubmatch(raw)
	if m == nil {
		return nil, fmt.Errorf("invalid requirement %q (want e.g. \"unity >= 6000.0\")", raw)
	}
	return &Requirement{Type: m[1], Constraint: m[2], Version: m[3], Raw: raw}, nil
}

// Resolve maps a requirement to a concrete registered tool.
func (r *Registry) Resolve(req *Requirement) (*model.Tool, error) {
	candidates := []*model.Tool{}
	for _, t := range r.List() {
		if t.Disabled {
			continue
		}
		if strings.EqualFold(t.Type, req.Type) {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no tool of type %q registered (requirement %q); add it under tools: in the config or run detect", req.Type, req.Raw)
	}
	// Prefer configured versions matching the constraint; pick the highest.
	bestScore := -1
	var best *model.Tool
	for _, t := range candidates {
		score, err := scoreVersion(t.Version, req)
		if err != nil || score < 0 {
			continue
		}
		if score > bestScore {
			bestScore, best = score, t
		}
	}
	if best == nil {
		// Report what exists, to make the mismatch debuggable.
		var vs []string
		for _, t := range candidates {
			vs = append(vs, fmt.Sprintf("%s@%s", t.ID, t.Version))
		}
		return nil, fmt.Errorf("no %q tool satisfies %q (have: %s)", req.Type, req.Raw, strings.Join(vs, ", "))
	}
	return best, nil
}

// scoreVersion returns a sortable score, or -1 if the tool version does not
// satisfy the requirement.
func scoreVersion(toolVersion string, req *Requirement) (int, error) {
	if req.Version == "" {
		return 0, nil // any version
	}
	if toolVersion == "" {
		return -1, nil // unknown version cannot be trusted to satisfy
	}
	cmp := compareVersions(toolVersion, req.Version)
	ok := false
	switch req.Constraint {
	case "", "=":
		ok = cmp == 0
	case ">=":
		ok = cmp >= 0
	case ">":
		ok = cmp > 0
	case "<=":
		ok = cmp <= 0
	case "<":
		ok = cmp < 0
	case "^":
		ok = cmp >= 0 && sameMajor(toolVersion, req.Version)
	default:
		ok = false
	}
	if !ok {
		return -1, nil
	}
	return cmp + 1, nil // higher versions score higher
}

// compareVersions compares dotted numeric versions, tolerating non-numeric
// segments (compared lexically). Returns -1/0/1.
func compareVersions(a, b string) int {
	as := splitVer(a)
	bs := splitVer(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := "0", "0"
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		avn, aerr := parseNum(av)
		bvn, berr := parseNum(bv)
		if aerr == nil && berr == nil {
			if avn != bvn {
				if avn < bvn {
					return -1
				}
				return 1
			}
			continue
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVer(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == ' ' })
}

func parseNum(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func sameMajor(a, b string) bool {
	as, bs := splitVer(a), splitVer(b)
	if len(as) == 0 || len(bs) == 0 {
		return false
	}
	return as[0] == bs[0]
}

// ---------------------------------------------------------------------------
// Unity detection
// ---------------------------------------------------------------------------

// unityEditorPaths returns candidate Unity editor binary paths per OS.
func unityEditorPaths(uc *config.UnityConfig) []struct{ id, ver, exe string } {
	var out []struct{ id, ver, exe string }
	if uc != nil {
		for id, path := range uc.Editors {
			ver := strings.TrimPrefix(id, "unity-")
			out = append(out, struct{ id, ver, exe string }{id, ver, resolveUnityExe(path)})
		}
	}
	hub := defaultUnityHubDir()
	if hub == "" {
		return out
	}
	entries, err := os.ReadDir(hub)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ver := e.Name()
		id := "unity-" + ver
		if idExists(out, id) {
			continue
		}
		out = append(out, struct{ id, ver, exe string }{id, ver, resolveUnityExe(filepath.Join(hub, ver))})
	}
	return out
}

func idExists(out []struct{ id, ver, exe string }, id string) bool {
	for _, o := range out {
		if o.id == id {
			return true
		}
	}
	return false
}

func defaultUnityHubDir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Applications/Unity/Hub/Editor"
	case "windows":
		home, _ := os.UserHomeDir()
		if home == "" {
			return ""
		}
		return filepath.Join(home, "AppData", "Local", "Programs", "Unity", "Hub", "Editor") // standard Unity Hub layout
	default:
		return "/opt/Unity/Hub/Editor"
	}
}

// resolveUnityExe maps an editor root (or .app) to the launcher binary.
func resolveUnityExe(root string) string {
	if strings.HasSuffix(root, ".app") {
		return filepath.Join(root, "Contents", "MacOS", "Unity")
	}
	// root may already be the binary
	if strings.HasSuffix(root, "Unity") || strings.HasSuffix(root, "Unity.exe") {
		return root
	}
	// editor root: darwin .app inside, windows Unity.exe inside
	app := filepath.Join(root, "Unity.app")
	if st, err := os.Stat(app); err == nil && st.IsDir() {
		return filepath.Join(app, "Contents", "MacOS", "Unity")
	}
	return filepath.Join(root, "Unity.exe") // windows layout (harmless if missing)
}

// detectUnity builds the detector closure.
func detectUnity(uc *config.UnityConfig) func() []*model.Tool {
	return func() []*model.Tool {
		var out []*model.Tool
		for _, p := range unityEditorPaths(uc) {
			if p.exe == "" {
				continue
			}
			if _, err := os.Stat(p.exe); err != nil {
				continue
			}
			out = append(out, &model.Tool{
				ID:             p.id,
				Type:           "unity",
				Version:        p.ver,
				Executable:     p.exe,
				Capabilities:   []string{"version", "compile", "test.editmode", "test.playmode", "build", "run.batch"},
				Source:         "detected",
				VersionCommand: []string{"-version"},
			})
		}
		return out
	}
}

// ResolveUnityDefault applies cfg.Unity.DefaultVersion semantics: returns the
// editor exe for a ">=X" default when no explicit version was requested.
func (r *Registry) ResolveUnityDefault(uc *config.UnityConfig) (exe string, version string, err error) {
	if uc == nil || uc.DefaultVersion == "" {
		return "", "", fmt.Errorf("no default unity version configured (unity.default_version)")
	}
	req, err := ParseRequirement("unity " + uc.DefaultVersion)
	if err != nil {
		return "", "", err
	}
	t, err := r.Resolve(req)
	if err != nil {
		return "", "", err
	}
	return t.Executable, t.Version, nil
}
