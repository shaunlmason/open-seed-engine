package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, ".seed/template.lock", "# provenance\nrepo acme/open-seed\nversion v1.4.2\n")
	write(t, root, ".seed/agents/implementer.md", "---\nname: implementer\ndescription: Take a claimed card to review.\nrole: implementer\nrun-agent: claude\npermission: safe-edit\n---\n\n## Task\n\nImplement.\n")
	write(t, root, ".seed/agents/README.md", "# not a role\n")
	write(t, root, "skills/greet/SKILL.md", "# greet\n")
	write(t, root, "skills/greet/reference.md", "details\n")
	write(t, root, "skills/managed/thirdparty/SKILL.md", "# vendored\n")
	write(t, root, "skills/README.md", "# skills source of truth\n")
	return root
}

func write(t *testing.T, root, p, c string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}

func render(t *testing.T, root string) map[string]string {
	t.Helper()
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[filepath.ToSlash(f.Path)] = f.Content
	}
	return out
}

func TestManifestTakesVersionAndRepoFromTemplateLock(t *testing.T) {
	got := render(t, fixture(t))
	raw, ok := got[ManifestPath]
	if !ok {
		t.Fatalf("no plugin manifest rendered; got %v", keys(got))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	// One release coordinate feeds both channels, with the tag's leading
	// v stripped for semver.
	if m["version"] != "1.4.2" {
		t.Errorf("version = %v, want 1.4.2 (from template.lock)", m["version"])
	}
	if m["name"] != PluginName {
		t.Errorf("name = %v, want %s", m["name"], PluginName)
	}
	if m["repository"] != "https://github.com/acme/open-seed" {
		t.Errorf("repository = %v", m["repository"])
	}
	if !strings.HasSuffix(raw, "}\n") {
		t.Error("manifest should end with a trailing newline")
	}
}

func TestMarketplaceEntryCarriesNoVersion(t *testing.T) {
	got := render(t, fixture(t))
	raw, ok := got[MarketplacePath]
	if !ok {
		t.Fatalf("no marketplace catalog rendered; got %v", keys(got))
	}
	var c struct {
		Name    string           `json:"name"`
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != MarketplaceName {
		t.Errorf("marketplace name = %q", c.Name)
	}
	if len(c.Plugins) != 1 {
		t.Fatalf("want exactly one plugin entry, got %d", len(c.Plugins))
	}
	if c.Plugins[0]["source"] != "./"+Dir {
		t.Errorf("source = %v, want ./%s", c.Plugins[0]["source"], Dir)
	}
	// The docs warn plugin.json always wins and a stale entry version
	// silently masks it, so the version lives in exactly one place.
	if _, present := c.Plugins[0]["version"]; present {
		t.Error("marketplace entry must not carry a version")
	}
}

func TestManagedSkillsAreExcludedFromThePayload(t *testing.T) {
	got := render(t, fixture(t))
	if _, ok := got[Dir+"/skills/greet/SKILL.md"]; !ok {
		t.Error("local skill missing from the payload")
	}
	if _, ok := got[Dir+"/skills/greet/reference.md"]; !ok {
		t.Error("supporting file missing from the payload")
	}
	for p := range got {
		if strings.Contains(p, "thirdparty") || strings.Contains(p, "managed") {
			t.Errorf("%s: managed (third-party) skills must never be republished by this channel", p)
		}
	}
}

func TestRolesAreFannedOutUnchanged(t *testing.T) {
	root := fixture(t)
	got := render(t, root)
	src, err := os.ReadFile(filepath.Join(root, ".seed/agents/implementer.md"))
	if err != nil {
		t.Fatal(err)
	}
	// D8: dual-format sources, fanned out UNCHANGED. The plugin is one
	// more unchanged fan-out, never a transformed one.
	if got[Dir+"/agents/implementer.md"] != string(src) {
		t.Error("plugin role copy is not byte-identical to .seed/agents/implementer.md")
	}
	if _, ok := got[Dir+"/agents/README.md"]; ok {
		t.Error("README.md is not a role and must not be shipped as a subagent")
	}
}

func TestNoTemplateLockRendersNothing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "skills/greet/SKILL.md", "# greet\n")
	files, err := Files(root)
	if err != nil {
		t.Fatalf("a repo without template provenance must be a no-op, got %v", err)
	}
	if len(files) != 0 {
		t.Errorf("want no files, got %d", len(files))
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	root := fixture(t)
	a, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("length differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("render differs at %d: %q vs %q", i, a[i].Path, b[i].Path)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
