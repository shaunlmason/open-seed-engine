package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SettingsPath is the project-scope settings file Claude Code reads once a
// folder is trusted. It is control surface (D4.1), so `enable`/`disable`
// compose the edit and a human merges the reviewed diff.
const SettingsPath = ".claude/settings.json"

// commitSHA matches a full git object name. Short prefixes are not matched:
// they are plausible branch names, and a false refusal is worse than the
// report a moving ref already gets.
var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// QualifiedName is how the plugin is addressed in enabledPlugins.
const QualifiedName = PluginName + "@" + MarketplaceName

// Relation describes how the plugin channel's pin stands to the template
// channel's release. Only `behind` is a fault: the others are either
// aligned or a deliberate choice the operator made, and a check wired into
// `make check` must not forbid a deliberate choice.
type Relation string

const (
	RelationOff      Relation = "off"      // channel not enabled
	RelationAligned  Relation = "aligned"  // pin names the template release
	RelationAhead    Relation = "ahead"    // pin names a LATER release: capability-only update
	RelationBehind   Relation = "behind"   // pin names an EARLIER release: a stale pin
	RelationFloating Relation = "floating" // pin is a branch or other moving ref: tracks upstream
	RelationBroken   Relation = "unpinned" // enabled but nothing usable to compare
)

// Status is the offline view of the channel, from checked-in files only.
type Status struct {
	Enabled         bool     `json:"enabled"`
	PinnedRef       string   `json:"pinned_ref,omitempty"`
	PinnedRepo      string   `json:"pinned_repo,omitempty"`
	TemplateRepo    string   `json:"template_repo"`
	TemplateVersion string   `json:"template_version"`
	Relation        Relation `json:"relation"`
	Drifted         bool     `json:"drifted"`
	Detail          string   `json:"detail"`
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
	// A file whose entire content is the literal `null` unmarshals without
	// error and leaves the map nil, which would panic on the first write.
	if m == nil {
		return nil, fmt.Errorf("%s is `null`, not a JSON object — repair it before enabling the plugin channel", SettingsPath)
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
// ref selects what the marketplace tracks: "" means the template release
// in .seed/template.lock (the aligned default). Passing a later tag or a
// branch is how a repo takes capability updates ahead of its structural
// template, which `status --check` reports rather than refuses.
func Enable(root, ref string) (*Status, error) {
	c, err := Load(root)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		ref = c.Version
	}
	// A marketplace source pins by branch or tag only: unlike an individual
	// plugin source inside a marketplace, it has no `sha` field. A commit
	// SHA here would not resolve at install time, so refuse it up front
	// rather than writing a declaration that silently never loads.
	if commitSHA.MatchString(ref) {
		return nil, fmt.Errorf("--ref %s looks like a commit SHA, and a marketplace source pins by branch or tag only (no sha) — name a release tag or a branch", ref)
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
			"ref":    ref,
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
// untouched. It deliberately does not consult .seed/template.lock: opting
// out must succeed even when the provenance that Enable needed is missing
// or malformed.
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
//
// Missing or unreadable provenance is reported, never returned as an error.
// Enable must refuse without coordinates (it cannot invent them), but
// reporting and opting OUT have to keep working in exactly that broken
// state, or a repo with a damaged template.lock has no way back off the
// channel.
func Report(root string) (*Status, error) {
	s := &Status{}
	c, provErr := Load(root)
	if provErr == nil {
		s.TemplateRepo, s.TemplateVersion = c.Repo, c.Version
	}

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
		s.Relation = RelationOff
		s.Detail = "plugin channel not enabled (template channel only) — nothing to check"
	case provErr != nil:
		s.Relation, s.Drifted = RelationBroken, true
		s.Detail = fmt.Sprintf("%s is enabled but .seed/template.lock could not be read (%v), so the two channels cannot be compared — repair the lock, or run `seed plugin disable`",
			QualifiedName, provErr)
	case s.PinnedRef == "":
		s.Relation, s.Drifted = RelationBroken, true
		s.Detail = fmt.Sprintf("%s is enabled but no %s marketplace is declared in %s — run `seed plugin enable`",
			QualifiedName, MarketplaceName, SettingsPath)
	case s.PinnedRepo != c.Repo:
		s.Relation, s.Drifted = RelationBroken, true
		s.Detail = fmt.Sprintf("marketplace repo %q disagrees with .seed/template.lock repo %q", s.PinnedRepo, c.Repo)
	case s.PinnedRef == c.Version:
		s.Relation = RelationAligned
		s.Detail = fmt.Sprintf("both channels at %s (%s)", c.Version, c.Repo)
	default:
		s.Relation, s.Detail, s.Drifted = compare(s.PinnedRef, c.Version)
	}
	return s, nil
}

// compare classifies a pin that is not identical to the template release.
//
// A capability-only update is a legitimate operation: pointing the
// marketplace at a LATER release, or at a moving ref such as a branch, is
// how the plugin channel advances ahead of the structural template. This
// check runs inside `make check`, so treating either as a failure would
// forbid the very thing the channel exists to allow. Only a pin left
// BEHIND the template release is reported as drift, because that is the
// accidental case: a template upgrade landed and the pin was never moved.
func compare(pinned, template string) (Relation, string, bool) {
	if commitSHA.MatchString(pinned) {
		return RelationBroken, fmt.Sprintf("plugin channel pins %s, which is a commit SHA: a marketplace source pins by branch or tag only, so this declaration cannot resolve — re-run `seed plugin enable`", pinned), true
	}
	pv, pok := parseVersion(pinned)
	tv, tok := parseVersion(template)
	if !pok {
		// Offline, from a settings file alone, a branch and a non-semver
		// tag are indistinguishable: git records no such marker. So this
		// is reported, never asserted, and a pin that turns out to be an
		// immutable tag is visible in `seed plugin status` for a human to
		// judge. Resolving it properly needs the network (os-6eb32b94).
		return RelationFloating, fmt.Sprintf("plugin channel tracks %q while the template channel is at %s — treated as a moving ref and not compared; if it is actually an immutable tag, it will never advance",
			pinned, template), false
	}
	if !tok {
		return RelationBroken, fmt.Sprintf(".seed/template.lock version %q is not a vX.Y.Z tag, so the channels cannot be compared", template), true
	}
	if less(tv, pv) {
		return RelationAhead, fmt.Sprintf("plugin channel is at %s, ahead of the template channel's %s — a capability-only update; run `seed template upgrade` when you want the structure too",
			pinned, template), false
	}
	return RelationBehind, fmt.Sprintf("plugin channel is pinned to %s but the template channel has moved on to %s — the pin is stale; run `seed plugin enable` to re-pin, or set the ref deliberately",
		pinned, template), true
}

// parseVersion reads a vX.Y.Z tag. Anything else (a branch, a sha, a
// bare name) is not a release pin and is treated as a moving ref.
func parseVersion(ref string) ([3]int, bool) {
	var out [3]int
	body, ok := strings.CutPrefix(ref, "v")
	if !ok {
		return out, false
	}
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
