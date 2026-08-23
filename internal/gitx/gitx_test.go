package gitx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoFixture(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "--initial-branch=main"},
		{"config", "user.name", "t"},
		{"config", "user.email", "t@t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return &Repo{Dir: dir}
}

func commitFile(t *testing.T, r *Repo, path, content, msg string) string {
	t.Helper()
	full := filepath.Join(r.Dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Git("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", msg); err != nil {
		t.Fatal(err)
	}
	sha, err := r.Git("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestPlumbingRoundTrip(t *testing.T) {
	r := repoFixture(t)
	sha1 := commitFile(t, r, "a/x.txt", "one\n", "c1")
	sha2 := commitFile(t, r, "a/y.txt", "two\n", "c2")

	if got, okR := r.ResolveRef("HEAD"); !okR || got != sha2 {
		t.Fatalf("ResolveRef HEAD = %q %v", got, okR)
	}
	if _, okR := r.ResolveRef("refs/heads/ghost"); okR {
		t.Fatal("ghost ref resolved")
	}
	content, found, err := r.CatFile("HEAD", "a/x.txt")
	if err != nil || !found || content != "one\n" {
		t.Fatalf("CatFile: %q %v %v", content, found, err)
	}
	if _, found, _ := r.CatFile("HEAD", "a/none.txt"); found {
		t.Fatal("missing path found")
	}
	names, err := r.ListTree("HEAD", "a")
	if err != nil || len(names) != 2 {
		t.Fatalf("ListTree: %v %v", names, err)
	}
	if names, err := r.ListTree("HEAD", "nodir"); err != nil || len(names) != 0 {
		t.Fatalf("ListTree on a missing dir: %v %v", names, err)
	}
	if h, err := r.HashObject("payload"); err != nil || len(h) != 40 {
		t.Fatalf("HashObject: %q %v", h, err)
	}
	anc, err := r.IsAncestor(sha1, sha2)
	if err != nil || !anc {
		t.Fatalf("IsAncestor(sha1, sha2): %v %v", anc, err)
	}
	anc, err = r.IsAncestor(sha2, sha1)
	if err != nil || anc {
		t.Fatalf("IsAncestor(sha2, sha1): %v %v", anc, err)
	}

	// A RunError carries the command and its output.
	_, err = r.Git("rev-parse", "definitely-not-a-ref")
	var re *RunError
	if !errors.As(err, &re) || re.Error() == "" {
		t.Fatalf("RunError not surfaced: %v", err)
	}
}

func TestCommitTreeModesAndDeletes(t *testing.T) {
	r := repoFixture(t)
	base := commitFile(t, r, "keep.txt", "keep\n", "base")
	sha, err := r.CommitTree("HEAD", []string{base}, "engine commit", []Change{
		{Path: "new/file.txt", Content: "hello\n"},
		{Path: "bin/tool", Content: "#!/bin/sh\n", Mode: "100755"},
	})
	if err != nil {
		t.Fatal(err)
	}
	content, found, _ := r.CatFile(sha, "new/file.txt")
	if !found || content != "hello\n" {
		t.Fatalf("committed content: %q %v", content, found)
	}
	lsOut, err := r.Git("ls-tree", sha, "bin/tool")
	if err != nil || !strings.Contains(lsOut, "100755") {
		t.Fatalf("executable mode lost: %q %v", lsOut, err)
	}
	// Deletion in a follow-up commit.
	sha2, err := r.CommitTree(sha, []string{sha}, "delete", []Change{
		{Path: "new/file.txt", Delete: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := r.CatFile(sha2, "new/file.txt"); found {
		t.Fatal("deleted path still present")
	}
	if _, found, _ := r.CatFile(sha2, "keep.txt"); !found {
		t.Fatal("unrelated path lost")
	}
}

func TestPushFetchAndFFClassification(t *testing.T) {
	remote := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--bare", remote)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v %s", err, out)
	}
	r := repoFixture(t)
	if _, err := r.Git("remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	sha1 := commitFile(t, r, "f.txt", "1\n", "c1")
	if err := r.Push("origin", sha1, "seed-state"); err != nil {
		t.Fatalf("push: %v", err)
	}
	// A second, unrelated commit for the same branch is non-fast-forward.
	orphan, err := r.CommitTree("HEAD", nil, "orphan", []Change{{Path: "o.txt", Content: "o\n"}})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Push("origin", orphan, "seed-state")
	var nff *ErrNonFastForward
	if !errors.As(err, &nff) || nff.Error() == "" {
		t.Fatalf("non-FF push not classified: %v", err)
	}

	if err := r.FetchNoForce("origin", "seed-state", "refs/seed/local"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got, okR := r.ResolveRef("refs/seed/local"); !okR || got != sha1 {
		t.Fatalf("fetched ref: %q %v", got, okR)
	}
	err = r.FetchNoForce("origin", "no-such-branch", "refs/seed/none")
	var noRef *ErrNoRemoteRef
	if !errors.As(err, &noRef) || noRef.Error() == "" {
		t.Fatalf("missing remote branch not classified: %v", err)
	}

	// Tags: create one on the remote via a second clone-side push.
	if _, err := r.Git("tag", "seed-anchor/1", sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Git("push", "-q", "origin", "seed-anchor/1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Git("tag", "-d", "seed-anchor/1"); err != nil {
		t.Fatal(err)
	}
	if err := r.FetchTagsNoForce("origin", "seed-anchor/*"); err != nil {
		t.Fatalf("fetch tags: %v", err)
	}
	tags, err := r.ListTags("seed-anchor/*")
	if err != nil || len(tags) != 1 || tags[0] != "seed-anchor/1" {
		t.Fatalf("tags: %v %v", tags, err)
	}
}

func TestSmallGitxBranches(t *testing.T) {
	r := repoFixture(t)
	commitFile(t, r, "f.txt", "x\n", "c1")
	// CatFile on a bad treeish is an error, not a silent miss.
	if _, _, err := r.CatFile("not-a-ref", "f.txt"); err == nil {
		t.Fatal("bad treeish CatFile passed")
	}
	// IsAncestor with a bogus ref errors.
	if _, err := r.IsAncestor("bogus", "HEAD"); err == nil {
		t.Fatal("bogus IsAncestor passed")
	}
	// ListTags with no matches: empty, no error.
	tags, err := r.ListTags("none/*")
	if err != nil || len(tags) != 0 {
		t.Fatalf("ListTags empty: %v %v", tags, err)
	}
	// Push to a nonexistent remote classifies as a plain error.
	if err := r.Push("no-such-remote", "HEAD", "b"); err == nil {
		t.Fatal("push to missing remote passed")
	}
	// FetchTagsNoForce against a missing remote errors.
	if err := r.FetchTagsNoForce("no-such-remote", "x/*"); err == nil {
		t.Fatal("fetch tags from missing remote passed")
	}
}
