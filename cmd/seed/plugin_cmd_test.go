package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The opt-in surface end to end: off by default, enable writes a valid
// declaration, --check catches cross-channel drift (plan os-221f5929).
func TestCLIPluginChannel(t *testing.T) {
	root := cliFixture(t)
	writeF(t, root, ".seed/template.lock", "repo acme/open-seed\nversion v0.3.0\n")

	// A template-only repo is off, and --check passes trivially: enabling
	// the channel is additive, never a precondition.
	if code, out, errS := seedRun(t, "plugin", "status", "--check"); code != 0 {
		t.Fatalf("status --check on a template-only repo: %d %s %s", code, out, errS)
	}
	code, out, _ := seedRun(t, "plugin", "status")
	if code != 0 {
		t.Fatalf("status: %d %s", code, out)
	}
	var st struct {
		Enabled         bool   `json:"enabled"`
		Drifted         bool   `json:"drifted"`
		TemplateVersion string `json:"template_version"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if st.Enabled || st.Drifted || st.TemplateVersion != "v0.3.0" {
		t.Fatalf("unexpected status: %+v", st)
	}

	if code, out, _ := seedRun(t, "plugin", "enable"); code != 0 {
		t.Fatalf("enable: %d %s", code, out)
	}
	b, err := os.ReadFile(filepath.Join(root, ".claude/settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	settings := string(b)
	for _, want := range []string{"extraKnownMarketplaces", "enabledPlugins", "open-seed@open-seed", "v0.3.0", "acme/open-seed"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings.json missing %q:\n%s", want, settings)
		}
	}
	if code, _, _ := seedRun(t, "plugin", "status", "--check"); code != 0 {
		t.Fatal("freshly enabled channel should agree with template.lock")
	}

	// The template channel moves and the plugin pin does not: that is the
	// R8 drift the check exists to catch.
	writeF(t, root, ".seed/template.lock", "repo acme/open-seed\nversion v0.4.0\n")
	code, _, errS := seedRun(t, "plugin", "status", "--check")
	if code != 1 {
		t.Fatalf("stale pin should exit 1, got %d", code)
	}
	if !strings.Contains(errS, "drifted") {
		t.Errorf("drift message unclear: %s", errS)
	}
	// Re-enabling re-pins to the current release.
	if code, _, _ := seedRun(t, "plugin", "enable"); code != 0 {
		t.Fatal("re-enable failed")
	}
	if code, _, _ := seedRun(t, "plugin", "status", "--check"); code != 0 {
		t.Fatal("re-enable did not re-pin")
	}

	if code, _, _ := seedRun(t, "plugin", "disable"); code != 0 {
		t.Fatal("disable failed")
	}
	if code, _, _ := seedRun(t, "plugin", "status", "--check"); code != 0 {
		t.Fatal("a disabled channel should pass --check")
	}

	if code, _, _ := seedRun(t, "plugin"); code != exitUsage {
		t.Fatal("bare plugin not usage")
	}
	if code, _, _ := seedRun(t, "plugin", "bogus"); code != exitUsage {
		t.Fatal("unknown subcommand not usage")
	}
}

// Without provenance there are no coordinates to name, so the command
// refuses rather than inventing them.
func TestCLIPluginWithoutTemplateLock(t *testing.T) {
	root := cliFixture(t)
	if err := os.Remove(filepath.Join(root, ".seed/template.lock")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if code, out, _ := seedRun(t, "plugin", "enable"); code != 1 || !strings.Contains(out, "plugin_refused") {
		t.Fatalf("want a refusal envelope, got %d %s", code, out)
	}
}

// A typo must not pass as a clean drift check in somebody's CI.
func TestCLIPluginRejectsUnknownArguments(t *testing.T) {
	root := cliFixture(t)
	writeF(t, root, ".seed/template.lock", "repo acme/open-seed\nversion v0.3.0\n")
	for _, args := range [][]string{
		{"plugin", "status", "--chek"},
		{"plugin", "status", "extra"},
		{"plugin", "enable", "--check"},
		{"plugin", "disable", "now"},
	} {
		if code, out, _ := seedRun(t, args...); code != exitUsage {
			t.Errorf("%v: want usage exit, got %d %s", args, code, out)
		}
	}
	// The one accepted flag still works.
	if code, _, _ := seedRun(t, "plugin", "status", "--check"); code != 0 {
		t.Error("--check should still be accepted")
	}
}
