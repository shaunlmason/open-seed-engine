// Package validate lints the orchestration artifacts (open-seed R9: a
// shipped convention and its validator are one deliverable): guardrails
// (auto-merge intersection rule), team files (tier ceiling, unique
// priorities, non-overlapping scopes, human lead), role variants (body-hash
// identity: variance in binding, never in craft, §6), and plan files.
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
	Name        string        `yaml:"name"`
	Mission     string        `yaml:"mission"`
	Lead        string        `yaml:"lead"`
	Scope       []string      `yaml:"scope"`
	SharedScope []SharedScope `yaml:"shared_scope"`
	Backlog     Backlog       `yaml:"backlog"`
	Priority    int           `yaml:"priority"`
	Tier        string        `yaml:"tier"`
	Review      string        `yaml:"review"`
}

// SharedScope is §6's explicit overlap exception: a path both squads may
// touch, with exactly one owning squad whose gate governs merges into it.
type SharedScope struct {
	Path  string `yaml:"path"`
	Owner string `yaml:"owner"`
}

// Backlog is the squad's card filter (v2 routing): a card matches when it
// carries any of the listed labels. The empty filter matches nothing
// special: cards reach such a squad explicitly or via the core fallback.
type Backlog struct {
	Labels []string `yaml:"labels"`
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
// allowlist may intersect neither the control surface nor plans/**:
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

// LoadTeams parses every squad file under .seed/teams (the .example
// suffix keeps a file inert).
func LoadTeams(root string) ([]Team, []error) {
	dir := filepath.Join(root, ".seed", "teams")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{fmt.Errorf("teams: %w", err)}
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
	return teams, errs
}

// isFallbackScope reports a bare-wildcard scope: §6's "matches what
// nothing else claims" catch-all, exempt from pairwise overlap.
func isFallbackScope(g string) bool { return globPrefix(g) == "" }

// sharedScopeOwned reports whether some squad declares the overlapping
// pair under shared_scope with an owner that names a real squad.
func sharedScopeOwned(teams []Team, a, b string) bool {
	names := map[string]bool{}
	for _, t := range teams {
		names[t.Name] = true
	}
	for _, t := range teams {
		for _, s := range t.SharedScope {
			if !names[s.Owner] {
				continue
			}
			if prefixOverlap(s.Path, a) && prefixOverlap(s.Path, b) {
				return true
			}
		}
	}
	return false
}

// Teams checks tier ≤ ceiling, a named human lead, unique priorities, and
// non-overlapping scopes across squads (§6). Core's bare-`**` fallback is
// exempt from pairwise overlap (it necessarily intersects every scope),
// but two squads both claiming the bare wildcard is a violation, and an
// overlap of two specific scopes passes only under an owned shared_scope
// entry.
func Teams(root string) []error {
	g, err := loadGuardrails(root)
	if err != nil {
		return []error{fmt.Errorf("teams: %w", err)}
	}
	teams, errs := LoadTeams(root)
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
					switch {
					case isFallbackScope(a) && isFallbackScope(b):
						errs = append(errs, fmt.Errorf("squads %s and %s both claim the bare-wildcard fallback scope — only one catch-all squad may exist (§6)", teams[i].Name, teams[j].Name))
					case isFallbackScope(a) || isFallbackScope(b):
						// the catch-all necessarily intersects everything: exempt
					case prefixOverlap(a, b) && !sharedScopeOwned(teams, a, b):
						errs = append(errs, fmt.Errorf("squads %s and %s have overlapping scopes (%q vs %q) without an owned shared_scope entry (§6)", teams[i].Name, teams[j].Name, a, b))
					}
				}
			}
		}
	}
	return errs
}

// FallbackSquad names the squad claiming the bare-wildcard scope (core
// in the shipped template): the §6 "no card can be invisible" floor.
func FallbackSquad(teams []Team) string {
	for _, t := range teams {
		for _, s := range t.Scope {
			if isFallbackScope(s) {
				return t.Name
			}
		}
	}
	return ""
}

// ResolveSquad implements §6's routing order: explicit squad → the
// matching backlog filter with the lowest priority int → the fallback
// squad.
func ResolveSquad(explicit string, labels []string, teams []Team) string {
	if explicit != "" {
		return explicit
	}
	best, bestPri := "", int(^uint(0)>>1)
	for _, t := range teams {
		for _, want := range t.Backlog.Labels {
			for _, have := range labels {
				if want == have && t.Priority < bestPri {
					best, bestPri = t.Name, t.Priority
				}
			}
		}
	}
	if best != "" {
		return best
	}
	return FallbackSquad(teams)
}

// TeamsWarnings reports §6 advisories that must not fail the shipped
// template: once multi-squad is active (>1 squad), a squad reviewing via
// CODEOWNERS should have its lead present there.
func TeamsWarnings(root string) []string {
	teams, _ := LoadTeams(root)
	if len(teams) <= 1 {
		return nil
	}
	var co string
	for _, p := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if b, err := os.ReadFile(filepath.Join(root, p)); err == nil {
			co = string(b)
			break
		}
	}
	var warns []string
	for _, t := range teams {
		if t.Review != "codeowners" || t.Lead == "" {
			continue
		}
		if !strings.Contains(co, "@"+t.Lead) {
			warns = append(warns, fmt.Sprintf("squad %s reviews via codeowners but lead @%s is not in CODEOWNERS — its scope gate cannot bind (§6)", t.Name, t.Lead))
		}
	}
	return warns
}

// AncestryActive is the §6 activation literal: goal-ancestry checking
// wakes when more than one squad exists or any squad declares a mission.
func AncestryActive(teams []Team) bool {
	if len(teams) > 1 {
		return true
	}
	for _, t := range teams {
		if strings.TrimSpace(t.Mission) != "" {
			return true
		}
	}
	return false
}

// AncestryCard is the minimal card shape the ancestry check needs.
type AncestryCard struct {
	ID, Parent, State string
	Labels            []string
}

// AncestryWarnings reports open cards with no resolvable parent chain to
// a mission card (one labeled "mission"). Report, not refusal: §6's
// alignment mitigation. Inactive repos (single squad, no mission) get
// nothing.
func AncestryWarnings(teams []Team, cards []AncestryCard) []string {
	if !AncestryActive(teams) {
		return nil
	}
	byID := map[string]AncestryCard{}
	for _, c := range cards {
		byID[c.ID] = c
	}
	isMission := func(c AncestryCard) bool {
		for _, l := range c.Labels {
			if l == "mission" {
				return true
			}
		}
		return false
	}
	var warns []string
	for _, c := range cards {
		if c.State == "done" || c.State == "cancelled" || isMission(c) {
			continue
		}
		cur, seen, rooted := c, map[string]bool{}, false
		for cur.Parent != "" && !seen[cur.ID] {
			seen[cur.ID] = true
			p, okP := byID[cur.Parent]
			if !okP {
				break
			}
			if isMission(p) {
				rooted = true
				break
			}
			cur = p
		}
		if !rooted {
			warns = append(warns, fmt.Sprintf("card %s has no resolvable parent chain to a mission (§6 goal ancestry)", c.ID))
		}
	}
	return warns
}

// RoleVariants enforces §6: a role's variants (name.<variant>.md) may differ
// only in frontmatter: the body (the craft) must be hash-identical to the
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
