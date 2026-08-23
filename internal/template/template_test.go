package template

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/upgrade"
)

// The fixtures are fully offline: a local git repo with tagged template
// versions stands in for the remote (the fetch step takes a URL; file
// paths are URLs to git).

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

// fixture builds the upstream template repo (v0.1.0, v0.2.0) and a
// consumer instantiated from v0.1.0 with local edits. Returns
// (upstreamDir, consumerDir).
func fixture(t *testing.T) (string, string) {
	t.Helper()
	up := t.TempDir()
	run(t, up, "init", "-q", "-b", "main")
	run(t, up, "config", "uploadpack.allowAnySHA1InWant", "true")
	write(t, up, ".seed/version", "1\n")
	write(t, up, ".seed/template.lock", "repo example/template\nversion v0.1.0\n")
	write(t, up, "docs/handbook.md", "# handbook\nline one\nline two\nline three\n")
	write(t, up, "scripts/seed", "#!/bin/sh\necho v1\n")
	if err := os.Chmod(filepath.Join(up, "scripts/seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, up, "Makefile", "check:\n\ttrue\n")
	write(t, up, "OLD.md", "goes away in v0.2.0\n")
	write(t, up, "plans/upstream-plan.md", "upstream work order v1\n")
	run(t, up, "add", "-A")
	run(t, up, "commit", "-qm", "v0.1.0")
	run(t, up, "tag", "-a", "v0.1.0", "-m", "v0.1.0")

	// v0.2.0: handbook edited (top line, mergeable against a bottom-line
	// local edit), scripts/seed rewritten (conflicts with a local rewrite),
	// OLD.md deleted, NEW.md added, .seed/version bumped, upstream plan
	// changed (must never reach the consumer).
	write(t, up, ".seed/version", "2\n")
	write(t, up, ".seed/template.lock", "repo example/template\nversion v0.2.0\n")
	write(t, up, "docs/handbook.md", "# handbook v2\nline one\nline two\nline three\n")
	write(t, up, "scripts/seed", "#!/bin/sh\necho v2\n")
	write(t, up, "NEW.md", "new in v0.2.0\n")
	write(t, up, "plans/upstream-plan.md", "upstream work order v2\n")
	run(t, up, "rm", "-q", "OLD.md")
	run(t, up, "add", "-A")
	run(t, up, "commit", "-qm", "v0.2.0")
	run(t, up, "tag", "-a", "v0.2.0", "-m", "v0.2.0")

	// Consumer: a "Use this template" copy of v0.1.0, no upstream
	// history, provenance only in the lock, plus local edits.
	con := t.TempDir()
	run(t, con, "init", "-q", "-b", "main")
	files := run(t, up, "ls-tree", "-r", "--name-only", "v0.1.0")
	for _, f := range strings.Split(files, "\n") {
		content := run(t, up, "cat-file", "-p", "v0.1.0:"+f)
		write(t, con, f, content+"\n")
	}
	// ls-tree cat-file round-trip normalizes the trailing newline; rewrite
	// the two files whose exact bytes the assertions depend on.
	write(t, con, "docs/handbook.md", "# handbook\nline one\nline two\nline three\n")
	write(t, con, "scripts/seed", "#!/bin/sh\necho v1\n")
	write(t, con, ".seed/version", "1\n")
	write(t, con, ".seed/template.lock", "repo example/template\nversion v0.1.0\n")
	write(t, con, "OLD.md", "goes away in v0.2.0\n")
	write(t, con, "Makefile", "check:\n\ttrue\n")
	write(t, con, "plans/upstream-plan.md", "upstream work order v1\n")
	// Local edits: handbook bottom line (clean merge against the v2 top
	// edit), scripts/seed rewritten (conflict), a local-only file, and the
	// consumer's own plan (work product).
	write(t, con, "docs/handbook.md", "# handbook\nline one\nline two\nline three local\n")
	write(t, con, "scripts/seed", "#!/bin/sh\necho local\n")
	write(t, con, "LOCAL.md", "consumer-only\n")
	write(t, con, "plans/my-task.md", "consumer work order\n")
	run(t, con, "add", "-A")
	run(t, con, "commit", "-qm", "instantiated + local work")
	return up, con
}

func mustRun(t *testing.T, opts Options) *Result {
	t.Helper()
	res, err := Run(opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func branchFile(t *testing.T, con, branch, path string) (string, bool) {
	t.Helper()
	r := &gitx.Repo{Dir: con}
	content, found, err := r.CatFile(branch, path)
	if err != nil {
		t.Fatal(err)
	}
	return content, found
}

func TestUpgradeMergesOntoBranch(t *testing.T) {
	up, con := fixture(t)
	res := mustRun(t, Options{Root: con, To: "v0.2.0", GitURL: up})
	if res.Branch != "template-upgrade/v0.2.0" {
		t.Fatalf("branch: %+v", res)
	}

	// Clean merge: upstream top-line edit + local bottom-line edit.
	hb, _ := branchFile(t, con, res.Branch, "docs/handbook.md")
	if !strings.Contains(hb, "# handbook v2") || !strings.Contains(hb, "line three local") {
		t.Fatalf("handbook merge lost an edit:\n%s", hb)
	}
	for _, m := range res.Merged {
		if m == "docs/handbook.md" {
			goto merged
		}
	}
	t.Fatalf("handbook not reported merged: %+v", res.Merged)
merged:

	// Genuine conflict: both rewrote scripts/seed, markers staged.
	sc, _ := branchFile(t, con, res.Branch, "scripts/seed")
	if !strings.Contains(sc, "<<<<<<<") || !strings.Contains(sc, "echo local") || !strings.Contains(sc, "echo v2") {
		t.Fatalf("conflict markers missing:\n%s", sc)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "scripts/seed" {
		t.Fatalf("conflicts: %+v", res.Conflicts)
	}

	// Fast-forward take: NEW.md arrives; OLD.md (locally untouched) goes.
	if _, found := branchFile(t, con, res.Branch, "NEW.md"); !found {
		t.Fatal("NEW.md not taken from upstream")
	}
	if _, found := branchFile(t, con, res.Branch, "OLD.md"); found {
		t.Fatal("upstream-deleted OLD.md survived")
	}

	// Local-only file untouched; work products never merged.
	if c, found := branchFile(t, con, res.Branch, "LOCAL.md"); !found || c != "consumer-only\n" {
		t.Fatal("local-only file disturbed")
	}
	if c, found := branchFile(t, con, res.Branch, "plans/my-task.md"); !found || c != "consumer work order\n" {
		t.Fatal("consumer plan disturbed")
	}
	if c, _ := branchFile(t, con, res.Branch, "plans/upstream-plan.md"); strings.Contains(c, "v2") {
		t.Fatal("upstream plan content reached the consumer's work products")
	}

	// template.lock advanced: version moved, upstream commit stamped.
	lockC, _ := branchFile(t, con, res.Branch, ".seed/template.lock")
	if !strings.Contains(lockC, "version v0.2.0") {
		t.Fatalf("lock not advanced:\n%s", lockC)
	}
	upV2 := run(t, up, "rev-parse", "v0.2.0^{commit}")
	if !strings.Contains(lockC, "commit "+upV2) {
		t.Fatalf("lock lacks the upstream commit %s:\n%s", upV2, lockC)
	}

	// .seed/version bump surfaces the engine-upgrade warning.
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, ".seed/version changes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("version-bump warning missing: %+v", res.Notes)
	}

	// The working tree and HEAD are untouched; the result is one commit on
	// the new branch parented on HEAD.
	r := &gitx.Repo{Dir: con}
	if out, _ := r.Git("status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Fatalf("working tree touched:\n%s", out)
	}
	head, _ := r.Git("rev-parse", "HEAD")
	parent, _ := r.Git("rev-parse", res.Branch+"^")
	if strings.TrimSpace(parent) != strings.TrimSpace(head) {
		t.Fatal("result branch is not one commit on HEAD")
	}

	// Executable bit preserved on the conflicted script.
	mode := run(t, con, "ls-tree", res.Branch, "--", "scripts/seed")
	if !strings.HasPrefix(mode, "100755") {
		t.Fatalf("mode lost: %s", mode)
	}
}

func TestSecondHopUsesStampedCommit(t *testing.T) {
	up, con := fixture(t)
	res := mustRun(t, Options{Root: con, To: "v0.2.0", GitURL: up})
	// Adopt the staged result, then upgrade again to a v0.3.0 whose base
	// must be the stamped commit (the tag object is never re-resolved).
	run(t, con, "checkout", "-q", res.Branch)
	write(t, up, "NEW.md", "changed again in v0.3.0\n")
	run(t, up, "add", "-A")
	run(t, up, "commit", "-qm", "v0.3.0")
	run(t, up, "tag", "-a", "v0.3.0", "-m", "v0.3.0")
	res2 := mustRun(t, Options{Root: con, To: "v0.3.0", GitURL: up})
	if c, _ := branchFile(t, con, res2.Branch, "NEW.md"); !strings.Contains(c, "changed again") {
		t.Fatalf("second hop did not take the v0.3.0 change: %q", c)
	}
}

func TestDowngradeRefusedWithoutTo(t *testing.T) {
	up, con := fixture(t)
	write(t, con, ".seed/template.lock", "repo example/template\nversion v0.2.0\n")
	run(t, con, "add", "-A")
	run(t, con, "commit", "-qm", "already on v0.2.0")
	// An explicit --to older than the recorded version is a deliberate
	// rollback: allowed, but noted. (The implicit latest-is-older refusal
	// shares the CompareTags gate exercised here.)
	res, err := Run(Options{Root: con, To: "v0.1.0", GitURL: up})
	if err != nil {
		t.Fatalf("explicit rollback must be allowed: %v", err)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "rollback") {
		t.Fatalf("rollback note missing: %+v", res.Notes)
	}
}

func TestImplicitOlderLatestRefused(t *testing.T) {
	_, con := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.Header().Set("Location", srv0+"/example/template/releases/tag/v0.0.9")
			w.WriteHeader(http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	srv0 = srv.URL
	_, err := Run(Options{Root: con, BaseURL: srv.URL, Check: true})
	if err == nil || err.Code != upgrade.ExitRefused || !strings.Contains(err.Msg, "downgrade") {
		t.Fatalf("implicit downgrade not refused: %v", err)
	}
}

// srv0 lets the handler reference its own server URL for the redirect.
var srv0 string

func TestCheckCreatesNothing(t *testing.T) {
	up, con := fixture(t)
	res := mustRun(t, Options{Root: con, To: "v0.2.0", Check: true, GitURL: up})
	if res.UpToDate || res.Target != "v0.2.0" {
		t.Fatalf("check result: %+v", res)
	}
	r := &gitx.Repo{Dir: con}
	if _, exists := r.ResolveRef("refs/heads/template-upgrade/v0.2.0"); exists {
		t.Fatal("--check created the branch")
	}
	if out, _ := r.Git("status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Fatal("--check touched the working tree")
	}
}

func TestDirtyTreeRefused(t *testing.T) {
	up, con := fixture(t)
	write(t, con, "Makefile", "check:\n\tfalse\n")
	_, err := Run(Options{Root: con, To: "v0.2.0", GitURL: up})
	if err == nil || err.Code != upgrade.ExitRefused || !strings.Contains(err.Msg, "uncommitted") {
		t.Fatalf("dirty tree not refused: %v", err)
	}
}

func TestExistingBranchRefused(t *testing.T) {
	up, con := fixture(t)
	mustRun(t, Options{Root: con, To: "v0.2.0", GitURL: up})
	_, err := Run(Options{Root: con, To: "v0.2.0", GitURL: up})
	if err == nil || err.Code != upgrade.ExitRefused || !strings.Contains(err.Msg, "already exists") {
		t.Fatalf("existing branch not refused: %v", err)
	}
}

func TestUnreachableHostExits7(t *testing.T) {
	_, con := fixture(t)
	_, err := Run(Options{Root: con, BaseURL: "http://127.0.0.1:9", Check: true})
	if err == nil || err.Code != upgrade.ExitUnreachable {
		t.Fatalf("want exit 7, got %v", err)
	}
	if !strings.Contains(err.Name, "template_host") {
		t.Fatalf("error name: %v", err.Name)
	}
}

func TestDamagedLockRefused(t *testing.T) {
	_, con := fixture(t)
	write(t, con, ".seed/template.lock", "version v0.1.0\nversion v0.2.0\nrepo x/y\n")
	run(t, con, "add", "-A")
	run(t, con, "commit", "-qm", "damage")
	_, err := Run(Options{Root: con, To: "v0.2.0"})
	if err == nil || err.Code != upgrade.ExitRefused {
		t.Fatalf("damaged lock not refused: %v", err)
	}
}

func TestMissingTargetTagIsRefusal(t *testing.T) {
	up, con := fixture(t)
	_, err := Run(Options{Root: con, To: "v9.9.9", GitURL: up})
	if err == nil || err.Code != upgrade.ExitRefused || !strings.Contains(err.Msg, "no ref") {
		t.Fatalf("missing tag not refused: %v", err)
	}
}

func TestGitFetchUnreachable(t *testing.T) {
	_, con := fixture(t)
	// Point the recorded repo at a nonexistent local path: the resolver is
	// bypassed with an explicit --to, and the git fetch itself fails.
	write(t, con, ".seed/template.lock", "repo "+filepath.Join(t.TempDir(), "gone")+"\nversion v0.1.0\n")
	run(t, con, "add", "-A")
	run(t, con, "commit", "-qm", "break the repo line")
	_, err := Run(Options{Root: con, To: "v0.2.0"})
	if err == nil || err.Code != upgrade.ExitUnreachable {
		t.Fatalf("unreachable git host: want exit 7, got %v", err)
	}
}
