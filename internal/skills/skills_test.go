package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sourceRepo builds a local skill-source repo with skills/alpha and
// skills/beta, tagged v1; returns its path.
func sourceRepo(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	run(t, src, "init", "-q", "-b", "main")
	write(t, src, "skills/alpha/SKILL.md", "# alpha v1\n")
	write(t, src, "skills/alpha/helper.sh", "echo alpha\n")
	write(t, src, "skills/beta/SKILL.md", "# beta v1\n")
	run(t, src, "add", "-A")
	run(t, src, "commit", "-qm", "v1")
	run(t, src, "tag", "v1")
	return src
}

func root(t *testing.T, manifest string) string {
	t.Helper()
	r := t.TempDir()
	if manifest != "" {
		write(t, r, ManifestPath, manifest)
	}
	return r
}

func TestEmptyManifestNoop(t *testing.T) {
	r := root(t, "")
	rep, err := Install(r, true)
	if err != nil || len(rep.Installed) != 0 {
		t.Fatalf("empty manifest must be a frozen no-op: %v %+v", err, rep)
	}
}

func TestLockInstallRoundTrip(t *testing.T) {
	src := sourceRepo(t)
	r := root(t, "schema_version: \"1\"\nskills:\n  - {name: alpha, repo: "+src+", ref: v1, path: skills/alpha}\n  - {name: beta, repo: "+src+", ref: v1, path: skills/beta}\n")
	lock, err := LockAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Skills) != 2 || len(lock.Skills[0].Commit) != 40 || len(lock.Skills[0].SHA256) != 64 {
		t.Fatalf("lock shape: %+v", lock)
	}
	rep, err := Install(r, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Installed) != 2 {
		t.Fatalf("install: %+v", rep)
	}
	b, err := os.ReadFile(filepath.Join(r, ManagedDir, "alpha", "SKILL.md"))
	if err != nil || string(b) != "# alpha v1\n" {
		t.Fatalf("alpha content: %q %v", b, err)
	}
	// Idempotent: second install reports up to date, no refetch needed.
	rep, err = Install(r, true)
	if err != nil || !strings.Contains(strings.Join(rep.Installed, ","), "up to date") {
		t.Fatalf("second install: %v %+v", err, rep)
	}
}

func TestHashMismatchRefused(t *testing.T) {
	src := sourceRepo(t)
	r := root(t, "schema_version: \"1\"\nskills:\n  - {name: alpha, repo: "+src+", ref: v1, path: skills/alpha}\n")
	if _, err := LockAll(r); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLock(r)
	if err != nil {
		t.Fatal(err)
	}
	l.Skills[0].SHA256 = strings.Repeat("0", 64)
	writeLock(t, r, l)
	if _, err := Install(r, false); err == nil || !strings.Contains(err.Error(), "does not match the lock") {
		t.Fatalf("hash mismatch not refused: %v", err)
	}
}

func writeLock(t *testing.T, r string, l *Lock) {
	t.Helper()
	out := "{\n  \"schema_version\": \"1\",\n  \"skills\": [\n"
	for i, e := range l.Skills {
		sep := ""
		if i < len(l.Skills)-1 {
			sep = ","
		}
		out += "    {\"name\": \"" + e.Name + "\", \"repo\": \"" + e.Repo + "\", \"ref\": \"" + e.Ref + "\", \"path\": \"" + e.Path + "\", \"commit\": \"" + e.Commit + "\", \"sha256\": \"" + e.SHA256 + "\"}" + sep + "\n"
	}
	out += "  ]\n}\n"
	if err := os.WriteFile(filepath.Join(r, LockPath), []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenRefusesUnlockedEditAndDrift(t *testing.T) {
	src := sourceRepo(t)
	r := root(t, "schema_version: \"1\"\nskills:\n  - {name: alpha, repo: "+src+", ref: v1, path: skills/alpha}\n")
	if _, err := LockAll(r); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(r, true); err != nil {
		t.Fatal(err)
	}
	// Unlocked manifest edit: add a skill without re-locking.
	write(t, r, ManifestPath, "schema_version: \"1\"\nskills:\n  - {name: alpha, repo: "+src+", ref: v1, path: skills/alpha}\n  - {name: beta, repo: "+src+", ref: v1, path: skills/beta}\n")
	if _, err := Install(r, true); err == nil || !strings.Contains(err.Error(), "--frozen") {
		t.Fatalf("unlocked edit not refused: %v", err)
	}
	// Unfrozen install accepts after re-locking; then on-disk drift is
	// caught by --frozen.
	if _, err := LockAll(r); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(r, true); err != nil {
		t.Fatal(err)
	}
	write(t, r, filepath.Join(ManagedDir, "alpha", "SKILL.md"), "tampered\n")
	if _, err := Install(r, true); err != nil {
		// Drift is REPAIRED by reinstall (hash mismatch on disk → refetch);
		// a repair that cannot restore the pin refuses. Either way the
		// call must not silently accept tampered content under --frozen.
		if !strings.Contains(err.Error(), "frozen") && !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("drift neither repaired nor refused: %v", err)
		}
	} else {
		b, _ := os.ReadFile(filepath.Join(r, ManagedDir, "alpha", "SKILL.md"))
		if string(b) != "# alpha v1\n" {
			t.Fatalf("tampered content survived a frozen install: %q", b)
		}
	}
}

func TestManagedOnlyPruning(t *testing.T) {
	src := sourceRepo(t)
	r := root(t, "schema_version: \"1\"\nskills:\n  - {name: alpha, repo: "+src+", ref: v1, path: skills/alpha}\n")
	if _, err := LockAll(r); err != nil {
		t.Fatal(err)
	}
	// A local skill outside managed/ and a stale managed leftover.
	write(t, r, "skills/local-skill/SKILL.md", "mine\n")
	write(t, r, filepath.Join(ManagedDir, "stale", "SKILL.md"), "old\n")
	rep, err := Install(r, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pruned) != 1 || rep.Pruned[0] != "stale" {
		t.Fatalf("prune: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(r, "skills/local-skill/SKILL.md")); err != nil {
		t.Fatal("local skill outside managed/ was touched")
	}
}

func TestTreeHashOrderIndependent(t *testing.T) {
	a := map[string][]byte{"x": []byte("1"), "y": []byte("2")}
	b := map[string][]byte{"y": []byte("2"), "x": []byte("1")}
	if TreeHash(a) != TreeHash(b) {
		t.Fatal("hash depends on insertion order")
	}
	c := map[string][]byte{"x": []byte("1"), "y": []byte("3")}
	if TreeHash(a) == TreeHash(c) {
		t.Fatal("hash misses content change")
	}
}

func TestMissingRefRefused(t *testing.T) {
	src := sourceRepo(t)
	r := root(t, "schema_version: \"1\"\nskills:\n  - {name: alpha, repo: "+src+", ref: v9, path: skills/alpha}\n")
	if _, err := LockAll(r); err == nil || !strings.Contains(err.Error(), "unreachable or ref missing") {
		t.Fatalf("missing ref not refused: %v", err)
	}
}

// Compose (skillfold semantics, plan os-6f3104db as amended): a compose
// entry generates a new skill from an ordered use list: concatenated
// bodies with heading demotion, supporting files carried over, with
// existence/self-use/cycle refusals and compose-of-compose in
// topological order.
func TestComposeGeneratesSkill(t *testing.T) {
	src := sourceRepo(t)
	r := root(t, "schema_version: \"1\"\nskills:\n  - {name: alpha, repo: "+src+", ref: v1, path: skills/alpha}\n  - {name: beta, repo: "+src+", ref: v1, path: skills/beta}\ncompose:\n  - {name: duo, description: Both at once., use: [alpha, beta]}\n  - {name: trio, use: [duo, alpha]}\n")
	if _, err := LockAll(r); err != nil {
		t.Fatal(err)
	}
	rep, err := Install(r, true)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(rep.Installed, ",")
	if !strings.Contains(joined, "duo (composed)") || !strings.Contains(joined, "trio (composed)") {
		t.Fatalf("composed skills not reported: %+v", rep)
	}
	b, err := os.ReadFile(filepath.Join(r, ManagedDir, "duo", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	ia, ib := strings.Index(s, "## alpha v1"), strings.Index(s, "## beta v1")
	if !strings.HasPrefix(s, "# duo\n") || ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("compose body wrong (root heading, demotion, order):\n%s", s)
	}
	if !strings.Contains(s, "Both at once.") {
		t.Fatalf("description missing:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(r, ManagedDir, "duo", "helper.sh")); err != nil {
		t.Fatalf("supporting file not carried over: %v", err)
	}
	// compose-of-compose nests one level deeper.
	b, _ = os.ReadFile(filepath.Join(r, ManagedDir, "trio", "SKILL.md"))
	if !strings.Contains(string(b), "## duo") || !strings.Contains(string(b), "### alpha v1") {
		t.Fatalf("compose-of-compose not topological/demoted:\n%s", b)
	}
	// A second install regenerates and pruning keeps composed dirs.
	rep, err = Install(r, true)
	if err != nil || len(rep.Pruned) != 0 {
		t.Fatalf("second install pruned composed dirs: %v %+v", err, rep)
	}
}

func TestComposeRefusals(t *testing.T) {
	cases := map[string]string{
		"unknown use": "schema_version: \"1\"\ncompose:\n  - {name: x, use: [ghost]}\n",
		"self use":    "schema_version: \"1\"\ncompose:\n  - {name: x, use: [x]}\n",
		"cycle":       "schema_version: \"1\"\ncompose:\n  - {name: a, use: [b]}\n  - {name: b, use: [a]}\n",
		"collision":   "schema_version: \"1\"\nskills:\n  - {name: x, repo: r, ref: v1}\ncompose:\n  - {name: x, use: [x]}\n",
	}
	for name, manifest := range cases {
		if _, err := LoadManifest(root(t, manifest)); err == nil {
			t.Errorf("%s: not refused", name)
		}
	}
}

func TestDemoteLeavesFencesAlone(t *testing.T) {
	in := "# H1\n```\n# not a heading\n```\n## H2\n"
	want := "## H1\n```\n# not a heading\n```\n### H2\n"
	if got := demote(in); got != want {
		t.Fatalf("demote:\n%q\nwant\n%q", got, want)
	}
}

func TestGitURLAndFrontmatterHelpers(t *testing.T) {
	cases := map[string]string{
		"owner/name":         "https://github.com/owner/name.git",
		"https://host/x.git": "https://host/x.git",
		"/abs/path":          "/abs/path",
		"./rel/path":         "./rel/path",
	}
	for in, want := range cases {
		if got := gitURL(in); got != want {
			t.Errorf("gitURL(%q) = %q, want %q", in, got, want)
		}
	}
	fm := map[string]string{
		"---\nname: x\n---\nbody\n": "body\n",
		"no frontmatter here":       "no frontmatter here",
		"---\nunterminated":         "---\nunterminated",
	}
	for in, want := range fm {
		if got := stripFrontmatter(in); got != want {
			t.Errorf("stripFrontmatter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstallAndLockErrorPaths(t *testing.T) {
	// Bad lock JSON.
	r := root(t, "schema_version: \"1\"\nskills: []\n")
	write(t, r, LockPath, "{{{")
	if _, err := Install(r, false); err == nil {
		t.Fatal("bad lock installed")
	}
	// A locked source that no longer exists: install refuses at fetch.
	src := sourceRepo(t)
	r2 := root(t, "schema_version: \"1\"\nskills:\n  - {name: alpha, repo: "+src+", ref: v1, path: skills/alpha}\n")
	if _, err := LockAll(r2); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(src)
	if _, err := Install(r2, false); err == nil {
		t.Fatal("vanished source installed")
	}
	// A compose input without SKILL.md is refused at generation.
	src2 := t.TempDir()
	run(t, src2, "init", "-q", "-b", "main")
	write(t, src2, "skills/noskill/helper.sh", "echo\n")
	run(t, src2, "add", "-A")
	run(t, src2, "commit", "-qm", "v1")
	run(t, src2, "tag", "v1")
	r3 := root(t, "schema_version: \"1\"\nskills:\n  - {name: noskill, repo: "+src2+", ref: v1, path: skills/noskill}\ncompose:\n  - {name: c, use: [noskill]}\n")
	if _, err := LockAll(r3); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(r3, false); err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("compose without SKILL.md: %v", err)
	}
	// Unknown schema_version in the manifest.
	r4 := root(t, "schema_version: \"7\"\nskills: []\n")
	if _, err := LoadManifest(r4); err == nil {
		t.Fatal("wrong schema accepted")
	}
	// Comments-only manifest parses as empty.
	r5 := root(t, "# nothing but a comment\n")
	m, err := LoadManifest(r5)
	if err != nil || len(m.Skills) != 0 {
		t.Fatalf("comments-only manifest: %+v %v", m, err)
	}
}
