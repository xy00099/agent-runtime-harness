package registry

import (
	"testing"

	"github.com/xy00099/agent-runtime-harness/internal/config"
)

func cfgWithTools() *config.Config {
	cfg := config.Default()
	cfg.Normalize()
	cfg.Tools["unity-6000"] = config.ToolConfig{
		Type:       "unity",
		Version:    "6000.3",
		Executable: "/Applications/Unity/Hub/Editor/6000.3/Unity.app/Contents/MacOS/Unity",
	}
	cfg.Tools["unity-2022"] = config.ToolConfig{
		Type:       "unity",
		Version:    "2022.3.20",
		Executable: "/Applications/Unity/Hub/Editor/2022.3.20f1/Unity.app/Contents/MacOS/Unity",
	}
	cfg.Tools["java-17"] = config.ToolConfig{
		Type:       "java",
		Version:    "17.0.9",
		Executable: "/Library/Java/JavaVirtualMachines/jdk-17/bin/java",
	}
	return cfg
}

func TestResolveGreaterEqual(t *testing.T) {
	r := New(cfgWithTools())
	req, err := ParseRequirement("unity >= 6000.0")
	if err != nil {
		t.Fatal(err)
	}
	tool, err := r.Resolve(req)
	if err != nil {
		t.Fatal(err)
	}
	if tool.ID != "unity-6000" {
		t.Fatalf("want unity-6000, got %s", tool.ID)
	}
}

func TestResolveExactAndMissing(t *testing.T) {
	r := New(cfgWithTools())
	tool, err := r.Resolve(mustParse(t, "java = 17"))
	if err == nil && tool != nil {
		// 17.0.9 != 17 exactly; behavior: no match.
		t.Fatalf("exact 17 should not match 17.0.9 (got %s)", tool.ID)
	}
	if _, err := r.Resolve(mustParse(t, "unity >= 7000.0")); err == nil {
		t.Fatal("unsatisfiable requirement must error")
	}
	if _, err := r.Resolve(mustParse(t, "blender >= 4.0")); err == nil {
		t.Fatal("unknown type must error")
	}
}

func TestResolveAnyVersion(t *testing.T) {
	r := New(cfgWithTools())
	tool, err := r.Resolve(mustParse(t, "java"))
	if err != nil {
		t.Fatal(err)
	}
	if tool.ID != "java-17" {
		t.Fatalf("want java-17, got %s", tool.ID)
	}
}

func TestParseRequirementErrors(t *testing.T) {
	for _, bad := range []string{"", "unity >>", "!!!"} {
		if _, err := ParseRequirement(bad); err == nil {
			t.Fatalf("%q should fail to parse", bad)
		}
	}
}

func TestVersionComparison(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"6000.3", "6000.0", 1},
		{"2022.3.20", "2023.0", -1},
		{"17.0.9", "17.0.9", 0},
		{"6000.3.48f1", "6000.3", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s,%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestDisabledToolSkipped(t *testing.T) {
	cfg := cfgWithTools()
	tc := cfg.Tools["unity-6000"]
	tc.Disabled = true
	cfg.Tools["unity-6000"] = tc
	r := New(cfg)
	if _, err := r.Resolve(mustParse(t, "unity >= 6000.0")); err == nil {
		t.Fatal("disabled tool must not resolve")
	}
}

func mustParse(t *testing.T, s string) *Requirement {
	t.Helper()
	req, err := ParseRequirement(s)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
