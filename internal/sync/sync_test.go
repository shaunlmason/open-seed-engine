package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const agentsMD = `# AGENTS.md

Intro.

## Rules

<!-- seed:rules:begin — managed block, synced from rules/ by seed sync; do not edit inline -->
- stale rule to be replaced
<!-- seed:rules:end -->

Outro.
`

func setup(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(p, c string) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".seed/agents/reviewer.md", "---\nrole: reviewer\n---\n\n## Task\n\nReview.\n")
	write("skills/greet/SKILL.md", "# greet\n\nSay hi.\n")
	write("rules/00-a.md", "# Fragment A\n\n- rule one\n- rule two\n")
	write("rules/10-b.md", "- rule three\n")
	write("AGENTS.md", agentsMD)
	return root
}

func TestApplyThenCheckClean(t *testing.T) {
	root := setup(t)
	if errs := Check(root); len(errs) == 0 {
		t.Fatal("pre-sync check should report drift")
	}
	n, err := Apply(root)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("nothing written")
	}
	if errs := Check(root); len(errs) != 0 {
		t.Fatalf("post-sync drift: %v", errs)
	}
	// Idempotent.
	n, err = Apply(root)
	if err != nil || n != 0 {
		t.Fatalf("second apply wrote %d (err %v)", n, err)
	}
}

func TestFanOutTargetsAndManagedBlock(t *testing.T) {
	root := setup(t)
	if _, err := Apply(root); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		".claude/agents/reviewer.md",
		".claude/skills/greet/SKILL.md",
		".agents/skills/greet/SKILL.md",
	} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("missing fan-out %s", p)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	s := string(b)
	if strings.Contains(s, "stale rule") {
		t.Error("managed block not replaced")
	}
	for _, want := range []string{"- rule one", "- rule three", "Intro.", "Outro."} {
		if !strings.Contains(s, want) {
			t.Errorf("AGENTS.md missing %q", want)
		}
	}
	if strings.Index(s, "- rule one") > strings.Index(s, "- rule three") {
		t.Error("fragments out of filename order")
	}
}

func TestCheckCatchesFanOutEdit(t *testing.T) {
	root := setup(t)
	if _, err := Apply(root); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ".claude/agents/reviewer.md"), []byte("edited in the wrong place"), 0o644)
	if errs := Check(root); len(errs) != 1 || !strings.Contains(errs[0].Error(), "drifted") {
		t.Fatalf("drift not caught: %v", errs)
	}
}

// Managed skills (skills/managed/<name>, plan os-6f3104db) fan out at
// the same depth as local ones; a local skill with the same name wins.
func TestManagedSkillsFanOutFlat(t *testing.T) {
	root := setup(t)
	write := func(p, c string) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("skills/managed/shared/SKILL.md", "# shared\n\nFrom upstream.\n")
	write("skills/managed/shared/helper.sh", "echo shared\n")
	write("skills/managed/greet/SKILL.md", "# greet (managed)\n\nShould lose to local.\n")
	if _, err := Apply(root); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []string{".claude/skills", ".agents/skills"} {
		b, err := os.ReadFile(filepath.Join(root, dst, "shared", "SKILL.md"))
		if err != nil || !strings.Contains(string(b), "From upstream") {
			t.Fatalf("%s: managed skill not flat: %v %q", dst, err, b)
		}
		if _, err := os.Stat(filepath.Join(root, dst, "managed")); !os.IsNotExist(err) {
			t.Fatalf("%s: managed/ segment leaked into the fan-out", dst)
		}
		b, _ = os.ReadFile(filepath.Join(root, dst, "greet", "SKILL.md"))
		if !strings.Contains(string(b), "Say hi") {
			t.Fatalf("%s: local skill did not win the name collision: %q", dst, b)
		}
	}
}

func TestFanOutSelectivityAndAgentsEdges(t *testing.T) {
	root := setup(t)
	write := func(p, c string) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-matching entries in the agents dir are skipped, dirs too.
	write(".seed/agents/notes.txt", "not a role")
	os.MkdirAll(filepath.Join(root, ".seed/agents/subdir"), 0o755)
	// Skills dir containing a stray file (not a dir): skipped.
	write("skills/README.md", "not a skill dir")
	if _, err := Apply(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude/agents/notes.txt")); !os.IsNotExist(err) {
		t.Fatal("non-.md agent file fanned out")
	}
	if _, err := os.Stat(filepath.Join(root, ".claude/skills/README.md")); !os.IsNotExist(err) {
		t.Fatal("stray skills file fanned out")
	}

	// AGENTS.md with no managed markers: renderAgentsMD declares nothing
	// to do; sync stays clean.
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# AGENTS.md\n\nno markers here\n"), 0o644)
	if _, err := Apply(root); err != nil {
		t.Fatal(err)
	}
	// Missing AGENTS.md entirely.
	os.Remove(filepath.Join(root, "AGENTS.md"))
	if _, err := Apply(root); err != nil {
		t.Fatal(err)
	}
	// A rules fragment with no leading H1 keeps its first line.
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agentsMD), 0o644)
	write("rules/20-c.md", "- bare rule, no heading\n")
	if _, err := Apply(root); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if !strings.Contains(string(b), "bare rule, no heading") {
		t.Fatal("headingless fragment lost")
	}
}

func TestPlanSourceReadErrors(t *testing.T) {
	// .seed/agents as a regular file: the role fan-out read fails.
	r1 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(r1, ".seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r1, ".seed", "agents"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(r1); err == nil {
		t.Fatal("agents-as-file tolerated")
	}
	if _, err := Apply(r1); err == nil {
		t.Fatal("apply over broken sources passed")
	}
	if errs := Check(r1); len(errs) == 0 {
		t.Fatal("check over broken sources passed")
	}

	// skills as a regular file: the skills fan-out read fails.
	r2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(r2, "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(r2); err == nil {
		t.Fatal("skills-as-file tolerated")
	}

	// rules as a regular file behind marked AGENTS.md: fragments fail.
	r3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(r3, "AGENTS.md"), []byte(agentsMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r3, "rules"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(r3); err == nil {
		t.Fatal("rules-as-file tolerated")
	}
}

func TestRenderEdgesAndStrayFiles(t *testing.T) {
	// Begin marker with no newline after it: block is left alone.
	r1 := t.TempDir()
	oneLine := "<!-- seed:rules:begin --><!-- seed:rules:end -->"
	if err := os.WriteFile(filepath.Join(r1, "AGENTS.md"), []byte(oneLine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r1, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r1, "rules", "a.md"), []byte("# T\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	actions, err := Plan(r1)
	if err != nil || len(actions) != 0 {
		t.Fatalf("headerless marker rendered: %v %v", actions, err)
	}

	// Only empty fragments: the managed block is left alone too.
	r2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(r2, "AGENTS.md"), []byte(agentsMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r2, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r2, "rules", "empty.md"), []byte("# Just a heading\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r2, "rules", "notes.txt"), []byte("not md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	actions, err = Plan(r2)
	if err != nil || len(actions) != 0 {
		t.Fatalf("empty fragments rendered: %v %v", actions, err)
	}

	// Stray plain files in skills/ and skills/managed/ are skipped.
	r3 := t.TempDir()
	for p, c := range map[string]string{
		"skills/README.md":            "stray",
		"skills/managed/README.md":    "stray",
		"skills/real/SKILL.md":        "s",
		"skills/managed/got/SKILL.md": "m",
	} {
		full := filepath.Join(r3, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actions, err = Plan(r3)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if strings.Contains(a.Path, "README") || strings.Contains(a.Path, "managed") {
			t.Fatalf("stray or unflattened path fanned out: %s", a.Path)
		}
	}
	if len(actions) != 4 { // real + got, each to two harness dirs
		t.Fatalf("actions: %v", actions)
	}

	// A fan-out destination occupied by a regular file: Apply fails.
	r4 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(r4, ".seed", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r4, ".seed", "agents", "dev.md"), []byte("---\nrole: dev\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r4, ".claude"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(r4); err == nil {
		t.Fatal("blocked destination tolerated")
	}
}
