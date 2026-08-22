// Package validate lints the orchestration artifacts (open-seed R9: a
// shipped convention and its validator are one deliverable): guardrails
// (auto-merge intersection rule), team files (tier ceiling, unique
// priorities, non-overlapping scopes, human lead), role variants (body-hash
// identity — variance in binding, never in craft, §6), and plan files.
package validate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/shaunlmason/open-seed-engine/internal/plan"
)

type Guardrails struct {
	Autonomy struct {
		DefaultTier string `yaml:"default_tier"`
		MaxTier     string `yaml:"max_tier"`
	} `yaml:"autonomy"`
	ProtectedPaths     []string `yaml:"protected_paths"`
	AutoMergeAllowlist []string `yaml:"auto_merge_allowlist"`
}

type Team struct {
	Name     string   `yaml:"name"`
	Lead     string   `yaml:"lead"`
	Scope    []string `yaml:"scope"`
	Priority int      `yaml:"priority"`
	Tier     string   `yaml:"tier"`
	Review   string   `yaml:"review"`
}

func tierRank(t string) int {
	switch strings.TrimSpace(t) {
	case "L1":
		return 1
	case "L2":
		return 2
	case "L3":
		return 3
	}
	return -1
}

// globPrefix reduces a glob to its literal prefix (up to the first
// wildcard), used for the conservative overlap test: two patterns overlap if
// either literal prefix is a prefix of the other. Conservative = may flag
// non-overlapping exotic globs, never misses a real overlap on the simple
// prefix globs the template uses.
func globPrefix(g string) string {
	if i := strings.IndexAny(g, "*?["); i >= 0 {
		g = g[:i]
	}
	return strings.TrimSuffix(g, "/")
}

func prefixOverlap(a, b string) bool {
	pa, pb := globPrefix(a), globPrefix(b)
	if pa == "" || pb == "" {
		return true // a bare wildcard overlaps everything
	}
	return strings.HasPrefix(pa+"/", pb+"/") || strings.HasPrefix(pb+"/", pa+"/")
}

// Repo runs all Phase 4 validators against a repo root and returns every
// finding (empty = clean).
func Repo(root string) []error {
	var errs []error
	errs = append(errs, GuardrailsFile(root)...)
	errs = append(errs, Teams(root)...)
	errs = append(errs, RoleVariants(root)...)
	errs = append(errs, Plans(root)...)
	return errs
}

func loadGuardrails(root string) (*Guardrails, error) {
	b, err := os.ReadFile(filepath.Join(root, ".seed", "guardrails.yaml"))
	if err != nil {
		return nil, err
	}
	var g Guardrails
	if err := yaml.Unmarshal(b, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// GuardrailsFile checks the D3/D4 intersection rule: the auto-merge
// allowlist may intersect neither the control surface nor plans/** —
// otherwise an agent could approve its own work order.
func GuardrailsFile(root string) []error {
	g, err := loadGuardrails(root)
	if err != nil {
		return []error{fmt.Errorf("guardrails: %w", err)}
	}
	var errs []error
	if tierRank(g.Autonomy.DefaultTier) < 0 || tierRank(g.Autonomy.MaxTier) < 0 {
		errs = append(errs, fmt.Errorf("guardrails: autonomy tiers must be L1/L2/L3 (default=%q max=%q)", g.Autonomy.DefaultTier, g.Autonomy.MaxTier))
	} else if tierRank(g.Autonomy.DefaultTier) > tierRank(g.Autonomy.MaxTier) {
		errs = append(errs, fmt.Errorf("guardrails: default_tier %s exceeds max_tier %s", g.Autonomy.DefaultTier, g.Autonomy.MaxTier))
	}
	forbidden := append(append([]string{}, g.ProtectedPaths...), "plans/**")
	for _, e := range g.AutoMergeAllowlist {
		for _, p := range forbidden {
			if prefixOverlap(e, p) {
				errs = append(errs, fmt.Errorf("guardrails: auto_merge_allowlist entry %q intersects %q — control surface and plans/** are never auto-mergeable", e, p))
			}
		}
	}
	return errs
}

// Teams checks tier ≤ ceiling, a named human lead, unique priorities, and
// non-overlapping scopes across squads (§6).
func Teams(root string) []error {
	g, err := loadGuardrails(root)
	if err != nil {
		return []error{fmt.Errorf("teams: %w", err)}
	}
	dir := filepath.Join(root, ".seed", "teams")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []error{fmt.Errorf("teams: %w", err)}
	}
	var errs []error
	var teams []Team
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var t Team
		if err := yaml.Unmarshal(b, &t); err != nil {
			errs = append(errs, fmt.Errorf("teams/%s: %w", e.Name(), err))
			continue
		}
		teams = append(teams, t)
	}
	priorities := map[int]string{}
	for _, t := range teams {
		if t.Lead == "" {
			errs = append(errs, fmt.Errorf("squad %s: no human lead — every squad has one by schema (§6)", t.Name))
		}
		if tierRank(t.Tier) < 0 || tierRank(t.Tier) > tierRank(g.Autonomy.MaxTier) {
			errs = append(errs, fmt.Errorf("squad %s: tier %q exceeds guardrails max_tier %s", t.Name, t.Tier, g.Autonomy.MaxTier))
		}
		if prev, dup := priorities[t.Priority]; dup {
			errs = append(errs, fmt.Errorf("squads %s and %s share priority %d — priorities must be unique (§6 routing)", prev, t.Name, t.Priority))
		}
		priorities[t.Priority] = t.Name
	}
	for i := 0; i < len(teams); i++ {
		for j := i + 1; j < len(teams); j++ {
			for _, a := range teams[i].Scope {
				for _, b := range teams[j].Scope {
					if prefixOverlap(a, b) {
						errs = append(errs, fmt.Errorf("squads %s and %s have overlapping scopes (%q vs %q) — scopes may not overlap (§6)", teams[i].Name, teams[j].Name, a, b))
					}
				}
			}
		}
	}
	return errs
}

// RoleVariants enforces §6: a role's variants (name.<variant>.md) may differ
// only in frontmatter — the body (the craft) must be hash-identical to the
// canonical name.md.
func RoleVariants(root string) []error {
	dir := filepath.Join(root, ".seed", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []error{fmt.Errorf("roles: %w", err)}
	}
	bodyHash := map[string]string{} // file -> body hash
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return []error{err}
		}
		body := roleBody(string(b))
		sum := sha256.Sum256([]byte(body))
		bodyHash[e.Name()] = hex.EncodeToString(sum[:])
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var errs []error
	for _, n := range names {
		base := strings.TrimSuffix(n, ".md")
		parts := strings.SplitN(base, ".", 2)
		if len(parts) != 2 {
			continue // canonical file
		}
		canonical := parts[0] + ".md"
		ch, ok := bodyHash[canonical]
		if !ok {
			errs = append(errs, fmt.Errorf("role variant %s has no canonical %s", n, canonical))
			continue
		}
		if bodyHash[n] != ch {
			errs = append(errs, fmt.Errorf("role variant %s body differs from %s — variance is allowed in binding (frontmatter), never in craft (§6)", n, canonical))
		}
	}
	return errs
}

func roleBody(content string) string {
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return content
	}
	_, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return content
	}
	return strings.TrimSpace(body)
}

// Plans lints every plan file in plans/ (README excluded).
func Plans(root string) []error {
	dir := filepath.Join(root, "plans")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no plans dir yet is fine
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, lintErr := range plan.Lint(string(b)) {
			errs = append(errs, fmt.Errorf("plans/%s: %v", e.Name(), lintErr))
		}
	}
	return errs
}
