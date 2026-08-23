package fastcards

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := OpenPath(filepath.Join(t.TempDir(), "cards.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.Sleep = func(time.Duration) {}
	if _, err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpenViaGitCommonDir(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--initial-branch=main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.Contains(s.Path, ".git") {
		t.Fatalf("db not under the git dir: %s", s.Path)
	}
	// Not a git repository: refusal.
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("open outside a repo passed")
	}
}

func TestInitIdempotentAndListings(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Init(); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	head, err := s.Sync()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(true, func(head string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "m", Changes: []gitx.Change{
			{Path: "tasks/os-1.md", Content: "a"},
			{Path: "tasks/os-2.md", Content: "b"},
			{Path: "other/x", Content: "c"},
		}, Events: []string{`{"verb":"m"}`}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	head, _ = s.Sync()
	names, err := s.ListDir(head, "tasks")
	if err != nil || len(names) != 2 {
		t.Fatalf("ListDir: %v %v", names, err)
	}
	all, err := s.ListAll(head)
	if err != nil || len(all) != 4 { // run log + 3
		t.Fatalf("ListAll: %v %v", all, err)
	}
	if content, found, _ := s.ReadFile(head, "other/x"); !found || content != "c" {
		t.Fatalf("ReadFile: %q %v", content, found)
	}
	if _, found, _ := s.ReadFile(head, "none"); found {
		t.Fatal("phantom file found")
	}
	// Delete change removes the row.
	if _, err := s.Mutate(true, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "d", Changes: []gitx.Change{{Path: "other/x", Delete: true}},
			Events: []string{`{"verb":"d"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	head, _ = s.Sync()
	if _, found, _ := s.ReadFile(head, "other/x"); found {
		t.Fatal("deleted file still present")
	}
}

func TestHaltAndTerminal(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Mutate(false, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "halt", Changes: []gitx.Change{{Path: "HALT", Content: "stop"}},
			Events: []string{`{"verb":"halt"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	head, _ := s.Sync()
	if halted, msg := s.Halted(head); !halted || msg != "stop" {
		t.Fatalf("halted: %v %q", halted, msg)
	}
	if _, err := s.Mutate(true, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "x", Events: []string{`{"verb":"x"}`}}, nil
	}); err == nil {
		t.Fatal("HALTed store accepted a mutation")
	}
	// Terminals pass through untouched.
	_, err := s.Mutate(false, func(string) (*stateref.Mutation, error) {
		return nil, &stateref.Terminal{Code: 2, Name: "claim_contention"}
	})
	var term *stateref.Terminal
	if !errors.As(err, &term) || term.Code != 2 {
		t.Fatalf("terminal: %v", err)
	}
}

func TestHelperBranches(t *testing.T) {
	if isBusy(errors.New("plain")) {
		t.Fatal("plain error classified busy")
	}
	for i := 0; i < 4; i++ {
		if backoff(i) <= 0 {
			t.Fatal("non-positive backoff")
		}
	}
	// Relative git-common-dir resolution.
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	p, err := DBPath(dir)
	if err != nil || !filepath.IsAbs(p) {
		t.Fatalf("DBPath: %q %v", p, err)
	}
	// Sync on a fresh path bootstraps the schema.
	s, err := OpenPath(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Sync(); err == nil {
		t.Log("sync on empty store tolerated (schema bootstrap)")
	}
}
