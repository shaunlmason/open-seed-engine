package stateref

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// origin + one working clone wired to it.
func storePair(t *testing.T) (string, *Store) {
	t.Helper()
	origin := t.TempDir()
	gitCmd(t, ".", "init", "-q", "--bare", origin)
	work := t.TempDir()
	gitCmd(t, ".", "init", "-q", "--initial-branch=main", work)
	gitCmd(t, work, "remote", "add", "origin", origin)
	s := Open(work, "origin", "seed-state")
	s.Sleep = func(time.Duration) {}
	return origin, s
}

func TestDirectInitSyncMutateAndListings(t *testing.T) {
	origin, s := storePair(t)

	// Sync before init: the remote ref does not exist.
	if _, err := s.Sync(); err == nil {
		t.Fatal("sync before init passed")
	}

	head1, err := s.Init()
	if err != nil {
		t.Fatal(err)
	}
	// Second init adopts the existing ref rather than recreating it.
	head2, err := s.Init()
	if err != nil || head2 != head1 {
		t.Fatalf("re-init: %q vs %q (%v)", head2, head1, err)
	}

	head, err := s.Mutate(true, func(head string) (*Mutation, error) {
		return &Mutation{
			Message: "add files",
			Changes: []gitx.Change{
				{Path: "tasks/os-1.md", Content: "x"},
				{Path: "notes/a.txt", Content: "y"},
			},
			Events: []string{`{"verb":"test"}`},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if content, found, _ := s.ReadFile(head, "tasks/os-1.md"); !found || content != "x" {
		t.Fatalf("ReadFile: %q %v", content, found)
	}
	names, err := s.ListDir(head, "tasks")
	if err != nil || len(names) != 1 || names[0] != "os-1.md" {
		t.Fatalf("ListDir: %v %v", names, err)
	}
	all, err := s.ListAll(head)
	if err != nil || len(all) != 3 { // run-log + two files
		t.Fatalf("ListAll: %v %v", all, err)
	}
	if halted, _ := s.Halted(head); halted {
		t.Fatal("halted without a marker")
	}

	// A Terminal from build refuses without retry and propagates verbatim.
	_, err = s.Mutate(true, func(string) (*Mutation, error) {
		return nil, &Terminal{Code: 6, Name: "fenced_out"}
	})
	var term *Terminal
	if !errors.As(err, &term) || term.Error() != "fenced_out" {
		t.Fatalf("terminal not propagated: %v", err)
	}

	// Error() surfaces on IntegrityError too.
	ie := &IntegrityError{Reason: "r", Detail: "d"}
	if ie.Error() != "r: d" {
		t.Fatalf("IntegrityError.Error: %q", ie.Error())
	}
	_ = origin
}

func TestDirectPushRaceRetries(t *testing.T) {
	origin, a := storePair(t)
	if _, err := a.Init(); err != nil {
		t.Fatal(err)
	}
	// A second clone advances the remote behind a's back.
	workB := t.TempDir()
	gitCmd(t, ".", "init", "-q", "--initial-branch=main", workB)
	gitCmd(t, workB, "remote", "add", "origin", origin)
	b := Open(workB, "origin", "seed-state")
	b.Sleep = func(time.Duration) {}
	if _, err := b.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Mutate(true, func(string) (*Mutation, error) {
		return &Mutation{Message: "b wins", Changes: []gitx.Change{{Path: "b.txt", Content: "b"}},
			Events: []string{`{"verb":"b"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	// a's Mutate must lose the first push, refetch, rebuild, and land.
	head, err := a.Mutate(true, func(head string) (*Mutation, error) {
		return &Mutation{Message: "a follows", Changes: []gitx.Change{{Path: "a.txt", Content: "a"}},
			Events: []string{`{"verb":"a"}`}}, nil
	})
	if err != nil {
		t.Fatalf("racing mutate: %v", err)
	}
	for _, p := range []string{"a.txt", "b.txt"} {
		if _, found, _ := a.ReadFile(head, p); !found {
			t.Fatalf("%s missing after race", p)
		}
	}
}

func TestDirectHaltBlocksMutations(t *testing.T) {
	_, s := storePair(t)
	if _, err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mutate(false, func(string) (*Mutation, error) {
		return &Mutation{Message: "halt", Changes: []gitx.Change{{Path: "HALT", Content: "stop"}},
			Events: []string{`{"verb":"halt"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Mutate(true, func(string) (*Mutation, error) {
		return &Mutation{Message: "should refuse", Events: []string{`{"verb":"x"}`}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "halted") {
		t.Fatalf("HALTed store accepted a mutation: %v", err)
	}
}

func TestDirectSyncAnchorVerification(t *testing.T) {
	_, s := storePair(t)
	if _, err := s.Init(); err != nil {
		t.Fatal(err)
	}
	head, err := s.Mutate(true, func(string) (*Mutation, error) {
		return &Mutation{Message: "m", Changes: []gitx.Change{{Path: "f", Content: "v"}},
			Events: []string{`{"verb":"m"}`}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A good anchor on the state head: Sync verifies ancestry and passes.
	if _, err := s.Repo.Git("tag", "seed-anchor/20260101T000000", head); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Repo.Git("push", "-q", "origin", "seed-anchor/20260101T000000"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(); err != nil {
		t.Fatalf("sync with a valid anchor: %v", err)
	}

	// An anchor pointing OFF the state history: ancestry check refuses.
	orphan, err := s.Repo.CommitTree("", nil, "orphan", []gitx.Change{{Path: "o", Content: "o"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Repo.Git("tag", "seed-anchor/20260102T000000", orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Repo.Git("push", "-q", "origin", "seed-anchor/20260102T000000"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Sync()
	var ie *IntegrityError
	if !errors.As(err, &ie) || ie.Reason != "anchor_ancestry_failed" {
		t.Fatalf("bad anchor accepted: %v", err)
	}
}

func TestBackoffPositive(t *testing.T) {
	for i := 0; i < 4; i++ {
		if backoff(i) <= 0 {
			t.Fatal("non-positive backoff")
		}
	}
}
