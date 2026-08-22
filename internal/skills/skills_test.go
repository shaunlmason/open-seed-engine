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
