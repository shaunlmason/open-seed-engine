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
