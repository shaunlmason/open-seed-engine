package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SettingsPath is the project-scope settings file Claude Code reads once a
// folder is trusted. It is control surface (D4.1), so `enable`/`disable`
// compose the edit and a human merges the reviewed diff.
const SettingsPath = ".claude/settings.json"

// QualifiedName is how the plugin is addressed in enabledPlugins.
const QualifiedName = PluginName + "@" + MarketplaceName

// Status is the offline view of the channel, from checked-in files only.
type Status struct {
	Enabled         bool   `json:"enabled"`
	PinnedRef       string `json:"pinned_ref,omitempty"`
	PinnedRepo      string `json:"pinned_repo,omitempty"`
	TemplateRepo    string `json:"template_repo"`
	TemplateVersion string `json:"template_version"`
	Drifted         bool   `json:"drifted"`
	Detail          string `json:"detail"`
}

func settingsFile(root string) string { return filepath.Join(root, SettingsPath) }

// readSettings loads settings.json as a generic tree. A missing file is an
// empty tree, so `enable` works on a repo that has none yet.
func readSettings(root string) (map[string]any, error) {
	b, err := os.ReadFile(settingsFile(root))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", SettingsPath, err)
	}
	return m, nil
}

// writeSettings renders the tree as 2-space JSON with a trailing newline.
// Object keys are emitted in sorted order (encoding/json sorts map keys),
// so repeated runs are byte-stable and `enable` is idempotent.
func writeSettings(root string, m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	p := settingsFile(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

func objAt(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// Enable declares the marketplace and turns the plugin on, sourcing the
// repo and ref from .seed/template.lock.
//
// The doubled `source` key is the real Claude Code schema, not a typo: the
// value of extraKnownMarketplaces.<name> is an object whose `source` field
// is itself an object carrying its own `source` discriminator ("github",
// "directory", "url", ...). Flattening it produces a declaration Claude
// Code ignores.
func Enable(root string) (*Status, error) {
	c, err := Load(root)
	if err != nil {
		return nil, err
	}
	m, err := readSettings(root)
	if err != nil {
		return nil, err
	}
	markets := objAt(m, "extraKnownMarketplaces")
	if markets == nil {
		markets = map[string]any{}
	}
	markets[MarketplaceName] = map[string]any{
		"source": map[string]any{
			"source": "github",
			"repo":   c.Repo,
			"ref":    c.Version,
		},
	}
	m["extraKnownMarketplaces"] = markets

	enabled := objAt(m, "enabledPlugins")
	if enabled == nil {
		enabled = map[string]any{}
	}
	enabled[QualifiedName] = true
	m["enabledPlugins"] = enabled

	if err := writeSettings(root, m); err != nil {
		return nil, err
	}
	return Report(root)
}

// Disable removes exactly the two entries Enable added, dropping the
// containing objects when they empty and leaving every other setting
// untouched.
func Disable(root string) (*Status, error) {
	m, err := readSettings(root)
	if err != nil {
		return nil, err
	}
	if markets := objAt(m, "extraKnownMarketplaces"); markets != nil {
		delete(markets, MarketplaceName)
		if len(markets) == 0 {
			delete(m, "extraKnownMarketplaces")
		}
	}
	if enabled := objAt(m, "enabledPlugins"); enabled != nil {
		delete(enabled, QualifiedName)
		if len(enabled) == 0 {
			delete(m, "enabledPlugins")
		}
	}
	if err := writeSettings(root, m); err != nil {
		return nil, err
	}
	return Report(root)
}

// Report is the offline cross-channel drift check: when the channel is
// enabled, the ref it pins must name the same release as
// .seed/template.lock. Disagreement means the two distribution paths have
// come apart, which is exactly the R8 failure the channel exists to soften.
func Report(root string) (*Status, error) {
	c, err := Load(root)
	if err != nil {
		return nil, err
	}
	s := &Status{TemplateRepo: c.Repo, TemplateVersion: c.Version}

	m, err := readSettings(root)
	if err != nil {
		return nil, err
	}
	on := false
	if enabled := objAt(m, "enabledPlugins"); enabled != nil {
		if v, ok := enabled[QualifiedName].(bool); ok && v {
			on = true
		}
	}
	if markets := objAt(m, "extraKnownMarketplaces"); markets != nil {
		if mk := objAt(markets, MarketplaceName); mk != nil {
			if src := objAt(mk, "source"); src != nil {
				s.PinnedRef, _ = src["ref"].(string)
				s.PinnedRepo, _ = src["repo"].(string)
			}
		}
	}
	s.Enabled = on

	switch {
	case !s.Enabled:
		s.Detail = "plugin channel not enabled (template channel only) — nothing to check"
	case s.PinnedRef == "":
		s.Drifted = true
		s.Detail = fmt.Sprintf("%s is enabled but no %s marketplace is declared in %s — run `seed plugin enable`",
			QualifiedName, MarketplaceName, SettingsPath)
	case s.PinnedRepo != c.Repo:
		s.Drifted = true
		s.Detail = fmt.Sprintf("marketplace repo %q disagrees with .seed/template.lock repo %q", s.PinnedRepo, c.Repo)
	case s.PinnedRef != c.Version:
		s.Drifted = true
		s.Detail = fmt.Sprintf("plugin channel pins %s but the template channel is at %s — the two distribution paths have drifted; run `seed plugin enable` after upgrading, or `seed template upgrade` first",
			s.PinnedRef, c.Version)
	default:
		s.Detail = fmt.Sprintf("both channels at %s (%s)", c.Version, c.Repo)
	}
	return s, nil
}
