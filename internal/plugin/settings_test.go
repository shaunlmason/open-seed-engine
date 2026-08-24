package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func settingsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, ".seed/template.lock", "repo acme/open-seed\nversion v1.4.2\n")
	write(t, root, SettingsPath, `{
  "permissions": {
    "allow": [
      "Bash(scripts/seed *)"
    ],
    "deny": []
  }
}
`)
	return root
}

func readBack(t *testing.T, root string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, SettingsPath))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The doubled `source` key is the real schema. A test pins the exact
// nesting so it cannot be quietly "fixed" into a flatter shape that Claude
// Code would ignore.
func TestEnableWritesTheDoubledSourceNesting(t *testing.T) {
	root := settingsFixture(t)
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	m := readBack(t, root)

	markets, ok := m["extraKnownMarketplaces"].(map[string]any)
	if !ok {
		t.Fatal("no extraKnownMarketplaces")
	}
	mk, ok := markets[MarketplaceName].(map[string]any)
	if !ok {
		t.Fatalf("no %s marketplace", MarketplaceName)
	}
	src, ok := mk["source"].(map[string]any)
	if !ok {
		t.Fatal("marketplace entry must nest an object under `source`")
	}
	if src["source"] != "github" {
		t.Errorf("source.source = %v, want github", src["source"])
	}
	if src["repo"] != "acme/open-seed" {
		t.Errorf("source.repo = %v (must come from template.lock)", src["repo"])
	}
	if src["ref"] != "v1.4.2" {
		t.Errorf("source.ref = %v (must come from template.lock)", src["ref"])
	}
	enabled, ok := m["enabledPlugins"].(map[string]any)
	if !ok || enabled[QualifiedName] != true {
		t.Errorf("enabledPlugins[%s] not set: %v", QualifiedName, m["enabledPlugins"])
	}
}

func TestEnablePreservesUnrelatedSettingsAndIsIdempotent(t *testing.T) {
	root := settingsFixture(t)
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(root, SettingsPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readBack(t, root)["permissions"]; !ok {
		t.Error("enable dropped the permissions block")
	}
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(root, SettingsPath))
	if string(first) != string(second) {
		t.Error("enable is not idempotent")
	}
}

func TestDisableRemovesOnlyWhatEnableAdded(t *testing.T) {
	root := settingsFixture(t)
	before, err := os.ReadFile(filepath.Join(root, SettingsPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := Disable(root); err != nil {
		t.Fatal(err)
	}
	m := readBack(t, root)
	if _, ok := m["extraKnownMarketplaces"]; ok {
		t.Error("emptied extraKnownMarketplaces should be dropped")
	}
	if _, ok := m["enabledPlugins"]; ok {
		t.Error("emptied enabledPlugins should be dropped")
	}
	if _, ok := m["permissions"]; !ok {
		t.Error("disable dropped the permissions block")
	}
	// Same content, modulo the canonical 2-space rendering.
	var a, b any
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal([]byte(mustRead(t, root)), &b)
	if !jsonEqual(a, b) {
		t.Error("disable did not restore the original settings content")
	}
}

func TestDisableKeepsOtherMarketplacesAndPlugins(t *testing.T) {
	root := settingsFixture(t)
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	m := readBack(t, root)
	m["extraKnownMarketplaces"].(map[string]any)["other"] = map[string]any{"source": map[string]any{"source": "github", "repo": "x/y"}}
	m["enabledPlugins"].(map[string]any)["other-plugin@other"] = true
	if err := writeSettings(root, m); err != nil {
		t.Fatal(err)
	}
	if _, err := Disable(root); err != nil {
		t.Fatal(err)
	}
	m = readBack(t, root)
	markets, _ := m["extraKnownMarketplaces"].(map[string]any)
	if markets == nil || markets["other"] == nil {
		t.Error("disable removed an unrelated marketplace")
	}
	if _, gone := markets[MarketplaceName]; gone {
		t.Error("disable left its own marketplace behind")
	}
	enabled, _ := m["enabledPlugins"].(map[string]any)
	if enabled == nil || enabled["other-plugin@other"] != true {
		t.Error("disable removed an unrelated plugin")
	}
}

func TestReportOffWhenNotEnabled(t *testing.T) {
	s, err := Report(settingsFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled || s.Drifted {
		t.Errorf("template-only repo should be off and undrifted: %+v", s)
	}
}

func TestReportDetectsCrossChannelDrift(t *testing.T) {
	root := settingsFixture(t)
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	s, err := Report(root)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.Drifted {
		t.Fatalf("freshly enabled channel should agree with template.lock: %+v", s)
	}
	// The template channel moves; the plugin channel's pin does not.
	write(t, root, ".seed/template.lock", "repo acme/open-seed\nversion v1.5.0\n")
	s, err = Report(root)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Drifted {
		t.Errorf("stale marketplace ref must be reported as drift: %+v", s)
	}
	if s.PinnedRef != "v1.4.2" || s.TemplateVersion != "v1.5.0" {
		t.Errorf("unexpected coordinates: %+v", s)
	}
}

func TestReportFlagsEnabledWithoutMarketplace(t *testing.T) {
	root := settingsFixture(t)
	m := readBack(t, root)
	m["enabledPlugins"] = map[string]any{QualifiedName: true}
	if err := writeSettings(root, m); err != nil {
		t.Fatal(err)
	}
	s, err := Report(root)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Drifted {
		t.Errorf("plugin enabled with no marketplace declared is a broken opt-in: %+v", s)
	}
}

func mustRead(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, SettingsPath))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Opting out must work in exactly the broken state that makes people want
// out: provenance missing or malformed.
func TestDisableSucceedsWithoutProvenance(t *testing.T) {
	root := settingsFixture(t)
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".seed/template.lock")); err != nil {
		t.Fatal(err)
	}
	s, err := Disable(root)
	if err != nil {
		t.Fatalf("disable must not need template.lock: %v", err)
	}
	if s.Enabled {
		t.Error("channel still reported as enabled after disable")
	}
	m := readBack(t, root)
	if _, ok := m["enabledPlugins"]; ok {
		t.Error("disable did not remove the plugin entry")
	}
}

func TestReportFlagsEnabledWithUnreadableProvenance(t *testing.T) {
	root := settingsFixture(t)
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	write(t, root, ".seed/template.lock", "this is not a lock file\n")
	s, err := Report(root)
	if err != nil {
		t.Fatalf("report must degrade, not fail: %v", err)
	}
	if !s.Drifted {
		t.Error("an enabled channel with unreadable provenance cannot be verified and must be flagged")
	}
}

// A settings file whose whole content is `null` unmarshals to a nil map;
// writing into it would panic.
func TestNullSettingsIsRefusedNotPanicked(t *testing.T) {
	root := settingsFixture(t)
	write(t, root, SettingsPath, "null\n")
	if _, err := Enable(root, ""); err == nil {
		t.Fatal("`null` settings should be refused")
	}
}

// The channel exists so capabilities can advance ahead of the structural
// template. This check runs inside `make check`, so a deliberate
// capability-only pin must be REPORTED, never refused: refusing it would
// forbid the operation the channel is for.
func TestAheadAndFloatingRefsAreNotFailures(t *testing.T) {
	cases := []struct {
		name     string
		ref      string
		relation Relation
	}{
		{"later release", "v2.0.0", RelationAhead},
		{"later patch", "v1.4.3", RelationAhead},
		{"branch", "main", RelationFloating},
		{"channel branch", "release/stable", RelationFloating},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := settingsFixture(t) // template.lock is v1.4.2
			if _, err := Enable(root, tc.ref); err != nil {
				t.Fatal(err)
			}
			s, err := Report(root)
			if err != nil {
				t.Fatal(err)
			}
			if s.Relation != tc.relation {
				t.Errorf("relation = %q, want %q (%s)", s.Relation, tc.relation, s.Detail)
			}
			if s.Drifted {
				t.Errorf("a deliberate %s pin must not be reported as drift: %s", tc.relation, s.Detail)
			}
			if s.PinnedRef != tc.ref {
				t.Errorf("pinned_ref = %q, want %q", s.PinnedRef, tc.ref)
			}
		})
	}
}

// The accidental case still fails: a template upgrade landed and nobody
// moved the pin.
func TestStalePinIsStillDrift(t *testing.T) {
	root := settingsFixture(t)
	if _, err := Enable(root, ""); err != nil {
		t.Fatal(err)
	}
	write(t, root, ".seed/template.lock", "repo acme/open-seed\nversion v1.5.0\n")
	s, err := Report(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Relation != RelationBehind || !s.Drifted {
		t.Errorf("a pin left behind a template upgrade must be drift: relation=%q drifted=%v", s.Relation, s.Drifted)
	}
}

func TestEnableDefaultsToTheTemplateRelease(t *testing.T) {
	root := settingsFixture(t)
	s, err := Enable(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.PinnedRef != "v1.4.2" || s.Relation != RelationAligned {
		t.Errorf("default enable should align with template.lock: %+v", s)
	}
}

func TestVersionParsing(t *testing.T) {
	for _, ok := range []string{"v0.0.0", "v1.2.3", "v10.20.30"} {
		if _, good := parseVersion(ok); !good {
			t.Errorf("%q should parse as a release tag", ok)
		}
	}
	for _, bad := range []string{"main", "1.2.3", "v1.2", "v1.2.3.4", "v1.2.x", "vx.y.z", "", "release/stable"} {
		if _, good := parseVersion(bad); good {
			t.Errorf("%q should NOT parse as a release tag", bad)
		}
	}
}
