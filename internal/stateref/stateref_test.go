package stateref

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
)

func TestMain(m *testing.M) {
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "test"}, {"GIT_AUTHOR_EMAIL", "test@test"},
		{"GIT_COMMITTER_NAME", "test"}, {"GIT_COMMITTER_EMAIL", "test@test"},
	} {
		os.Setenv(kv[0], kv[1])
	}
	os.Exit(m.Run())
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func setup(t *testing.T) (origin string, a, b *Store) {
	t.Helper()
	origin = filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	mk := func(name string) *Store {
		dir := filepath.Join(t.TempDir(), name)
		mustGit(t, "", "init", "--initial-branch=main", dir)
		mustGit(t, dir, "remote", "add", "origin", origin)
		s := Open(dir, "origin", "seed-state")
		s.Sleep = func(time.Duration) {}
		return s
	}
	return origin, mk("a"), mk("b")
}

// The push-wins race (§7.1): A's build runs against a head that B advances
// before A pushes. A's push is rejected, the loop re-fetches, and the second
// build sees B's write: the loser decides from fresh state.
func TestMutateRetriesOnPushRace(t *testing.T) {
	_, a, b := setup(t)
	if _, err := a.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Init(); err != nil {
		t.Fatal(err)
	}

	attempts := 0
	sawFreshState := false
	_, err := a.Mutate(true, func(head string) (*Mutation, error) {
		attempts++
		if attempts == 1 {
			// B sneaks in a write between A's fetch and A's push.
			if _, err := b.Mutate(true, func(string) (*Mutation, error) {
				return &Mutation{Message: "b wins", Changes: []gitx.Change{{Path: "b.txt", Content: "b\n"}}}, nil
			}); err != nil {
				return nil, err
			}
		} else {
			// Second attempt must see B's write in the fresh head.
			_, found, _ := a.ReadFile(head, "b.txt")
			sawFreshState = found
		}
		return &Mutation{Message: "a attempt", Changes: []gitx.Change{{Path: "a.txt", Content: "a\n"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one rejected push, one retry)", attempts)
	}
	if !sawFreshState {
		t.Error("retry did not observe the winner's write")
	}
}

// A Terminal from build stops the loop immediately: contention decisions
// made on fresh state are not retried.
func TestMutateTerminalStopsLoop(t *testing.T) {
	_, a, _ := setup(t)
	if _, err := a.Init(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err := a.Mutate(true, func(string) (*Mutation, error) {
		calls++
		return nil, &Terminal{Code: 2, Name: "claim_contention"}
	})
	term, ok := err.(*Terminal)
	if !ok || term.Code != 2 || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestInitCreationRace(t *testing.T) {
	_, a, b := setup(t)
	ha, err := a.Init()
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.Init()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("init heads differ: %s vs %s", ha, hb)
	}
}
