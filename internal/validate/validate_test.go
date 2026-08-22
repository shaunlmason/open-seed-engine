package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodGuardrails = `autonomy:
  default_tier: L1
  max_tier: L2
protected_paths:
  - .seed/**
  - .github/**
  - CODEOWNERS
auto_merge_allowlist: []
`

const goodTeam = `name: core
lead: alice
scope: ["**"]
priority: 1
tier: L2
`

const roleA = `---
role: reviewer
run-agent: claude
---

## Task

Review things.

## Done When

- Reviewed.
`

func setup(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(p, c string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".seed/guardrails.yaml", goodGuardrails)
	write(".seed/teams/core.yaml", goodTeam)
	write(".seed/agents/reviewer.md", roleA)
	write(".seed/agents/reviewer.codex.md", strings.Replace(roleA, "claude", "codex", 1))
	return root
}

func hasError(errs []error, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}

func TestCleanRepoValidates(t *testing.T) {
	root := setup(t)
	if errs := Repo(root); len(errs) > 0 {
		t.Fatalf("clean repo: %v", errs)
	}
}

func TestAllowlistIntersectionRejected(t *testing.T) {
	root := setup(t)
	for _, entry := range []string{".seed/config.toml", "plans/**", "**"} {
		g := strings.Replace(goodGuardrails, "auto_merge_allowlist: []",
			"auto_merge_allowlist: [\""+entry+"\"]", 1)
		os.WriteFile(filepath.Join(root, ".seed/guardrails.yaml"), []byte(g), 0o644)
		if errs := GuardrailsFile(root); !hasError(errs, "intersects") {
			t.Errorf("allowlist %q accepted: %v", entry, errs)
		}
	}
	// A benign entry passes.
	g := strings.Replace(goodGuardrails, "auto_merge_allowlist: []",
		"auto_merge_allowlist: [\"docs/**\"]", 1)
	os.WriteFile(filepath.Join(root, ".seed/guardrails.yaml"), []byte(g), 0o644)
	if errs := GuardrailsFile(root); len(errs) > 0 {
		t.Errorf("benign allowlist rejected: %v", errs)
	}
}

func TestTierCeilingEnforced(t *testing.T) {
	root := setup(t)
	os.WriteFile(filepath.Join(root, ".seed/teams/core.yaml"),
		[]byte(strings.Replace(goodTeam, "tier: L2", "tier: L3", 1)), 0o644)
	if errs := Teams(root); !hasError(errs, "exceeds guardrails max_tier") {
		t.Fatalf("tier breach accepted: %v", errs)
	}
}

func TestMissingLeadRejected(t *testing.T) {
	root := setup(t)
	os.WriteFile(filepath.Join(root, ".seed/teams/core.yaml"),
		[]byte(strings.Replace(goodTeam, "lead: alice", "lead: \"\"", 1)), 0o644)
	if errs := Teams(root); !hasError(errs, "no human lead") {
		t.Fatalf("leadless squad accepted: %v", errs)
	}
}

func TestDuplicatePriorityAndScopeOverlap(t *testing.T) {
	root := setup(t)
	second := strings.NewReplacer("core", "web", "priority: 1", "priority: 1").Replace(goodTeam)
	os.WriteFile(filepath.Join(root, ".seed/teams/web.yaml"), []byte(second), 0o644)
	errs := Teams(root)
	if !hasError(errs, "share priority") {
		t.Errorf("duplicate priority accepted: %v", errs)
	}
	if !hasError(errs, "bare-wildcard fallback") {
		t.Errorf("double catch-all accepted: %v", errs)
	}
	// Disjoint scopes + unique priorities pass.
	second = strings.NewReplacer("core", "web", "priority: 1", "priority: 2", `["**"]`, `["web/**"]`).Replace(goodTeam)
	os.WriteFile(filepath.Join(root, ".seed/teams/web.yaml"), []byte(second), 0o644)
	os.WriteFile(filepath.Join(root, ".seed/teams/core.yaml"),
		[]byte(strings.Replace(goodTeam, `["**"]`, `["core/**"]`, 1)), 0o644)
	if errs := Teams(root); len(errs) > 0 {
		t.Errorf("disjoint squads rejected: %v", errs)
	}
}

func TestRoleVariantBodyDriftRejected(t *testing.T) {
	root := setup(t)
	drifted := strings.Replace(roleA, "Review things.", "Review things MY way.", 1)
	drifted = strings.Replace(drifted, "claude", "codex", 1)
	os.WriteFile(filepath.Join(root, ".seed/agents/reviewer.codex.md"), []byte(drifted), 0o644)
	if errs := RoleVariants(root); !hasError(errs, "never in craft") {
		t.Fatalf("body drift accepted: %v", errs)
	}
}

func TestOrphanVariantRejected(t *testing.T) {
	root := setup(t)
	os.WriteFile(filepath.Join(root, ".seed/agents/ghost.codex.md"), []byte(roleA), 0o644)
	if errs := RoleVariants(root); !hasError(errs, "no canonical") {
		t.Fatalf("orphan variant accepted: %v", errs)
	}
}

func TestPlanLintInRepo(t *testing.T) {
	root := setup(t)
	os.MkdirAll(filepath.Join(root, "plans"), 0o755)
	os.WriteFile(filepath.Join(root, "plans/os-1234.md"),
		[]byte("# Plan\n\n## Steps\n\n1. x\n"), 0o644)
	if errs := Plans(root); !hasError(errs, "missing or empty section") {
		t.Fatalf("bad plan accepted: %v", errs)
	}
}

// §6 as amended (plan os-10c10aae): core's bare-`**` fallback coexists
// with any second squad; two specific scopes overlap only under an owned
// shared_scope entry; the ancestry literal is >1 squad || any mission.
func TestFallbackExemptionAndSharedScope(t *testing.T) {
	root := setup(t)
	web := strings.NewReplacer("core", "web", "priority: 1", "priority: 2", `["**"]`, `["web/**"]`).Replace(goodTeam)
	os.WriteFile(filepath.Join(root, ".seed/teams/web.yaml"), []byte(web), 0o644)
	if errs := Teams(root); len(errs) > 0 {
		t.Fatalf("core ** + specific squad must validate: %v", errs)
	}
	// A second specific squad overlapping web/** fails without shared_scope…
	api := strings.NewReplacer("core", "api", "priority: 1", "priority: 3", `["**"]`, `["web/shared/**"]`).Replace(goodTeam)
	os.WriteFile(filepath.Join(root, ".seed/teams/api.yaml"), []byte(api), 0o644)
	if errs := Teams(root); !hasError(errs, "overlapping scopes") {
		t.Fatalf("specific overlap accepted: %v", errs)
	}
	// …and passes once an owned shared_scope entry covers it.
	api = strings.Replace(api, "scope:", "shared_scope:\n  - {path: \"web/shared/**\", owner: web}\nscope:", 1)
	os.WriteFile(filepath.Join(root, ".seed/teams/api.yaml"), []byte(api), 0o644)
	if errs := Teams(root); len(errs) > 0 {
		t.Fatalf("owned shared_scope still rejected: %v", errs)
	}
}

func TestAncestryActivationLiteral(t *testing.T) {
	solo := []Team{{Name: "core"}}
	if AncestryActive(solo) {
		t.Fatal("core-only, missionless repo must stay inactive")
	}
	if !AncestryActive([]Team{{Name: "core", Mission: "ship"}}) {
		t.Fatal("a mission activates ancestry")
	}
	if !AncestryActive([]Team{{Name: "a"}, {Name: "b"}}) {
		t.Fatal("a second squad activates ancestry")
	}
	cards := []AncestryCard{
		{ID: "m1", Labels: []string{"mission"}},
		{ID: "c1", Parent: "m1"},
		{ID: "c2"},
		{ID: "c3", State: "done"},
	}
	warns := AncestryWarnings([]Team{{Name: "core", Mission: "ship"}}, cards)
	if len(warns) != 1 || !strings.Contains(warns[0], "c2") {
		t.Fatalf("ancestry warnings = %v", warns)
	}
	if w := AncestryWarnings(solo, cards); w != nil {
		t.Fatalf("inactive repo warned: %v", w)
	}
}
