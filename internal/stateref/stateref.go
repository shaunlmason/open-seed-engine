// Package stateref implements the seed-state ref lifecycle (§7.2): the
// dedicated branch carrying machine-written coordination state, written only
// by this shim, one commit per verb, never checked out. Integrity posture is
// push-access-deep (R10) with the §7.2 mitigations: no-force fetches surface
// history rewrites, anchor-tag ancestry is verified on every sync, and a HALT
// marker at the ref root stops mutating verbs until a human resumes.
package stateref

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
)

const (
	localRef   = "refs/seed/state"
	haltPath   = "HALT"
	anchorGlob = "seed-anchor/*"
	runLogPath = "run-log.jsonl"
)

// IntegrityError is the halt+escalate incident (§7.2): observed rewrite,
// failed anchor ancestry, or a HALT marker on a mutating verb.
type IntegrityError struct {
	Reason string
	Detail string
}

func (e *IntegrityError) Error() string { return e.Reason + ": " + e.Detail }

// ErrContention is returned by a Mutate build func to signal a terminal,
// non-retryable refusal decided from fresh state (e.g. claim contention).
type Terminal struct {
	Code int
	Name string
	Data map[string]any
}

func (t *Terminal) Error() string { return t.Name }

type Store struct {
	Repo   *gitx.Repo
	Remote string
	Branch string

	MaxAttempts int
	// Sleep is swappable for tests.
	Sleep func(time.Duration)
}

func Open(repoDir, remote, branch string) *Store {
	return &Store{
		Repo:        &gitx.Repo{Dir: repoDir},
		Remote:      remote,
		Branch:      branch,
		MaxAttempts: 6,
		Sleep:       time.Sleep,
	}
}

// Init creates the orphan seed-state ref (empty tree + run log) and pushes
// it. A push rejected because the ref already exists resolves trivially:
// fetch and proceed (§7.2 bootstrap).
func (s *Store) Init() (string, error) {
	if err := s.fetch(); err == nil {
		head, _ := s.Repo.ResolveRef(localRef)
		return head, nil // already exists
	} else {
		var noRef *gitx.ErrNoRemoteRef
		if !errors.As(err, &noRef) {
			return "", err
		}
	}
	commit, err := s.Repo.CommitTree("", nil, "seed init: create state ref", []gitx.Change{
		{Path: runLogPath, Content: ""},
	})
	if err != nil {
		return "", err
	}
	if err := s.Repo.Push(s.Remote, commit, s.Branch); err != nil {
		var ff *gitx.ErrNonFastForward
		if errors.As(err, &ff) {
			// Creation race: someone else initialized first. Adopt theirs.
			if ferr := s.fetch(); ferr != nil {
				return "", ferr
			}
			head, _ := s.Repo.ResolveRef(localRef)
			return head, nil
		}
		return "", err
	}
	if _, err := s.Repo.Git("update-ref", localRef, commit); err != nil {
		return "", err
	}
	return commit, nil
}

func (s *Store) fetch() error {
	if err := s.Repo.FetchNoForce(s.Remote, s.Branch, localRef); err != nil {
		var ff *gitx.ErrNonFastForward
		if errors.As(err, &ff) {
			return &IntegrityError{Reason: "non_fast_forward", Detail: "seed-state history rewrite observed; refusing to adopt (§7.2). Escalate to a human."}
		}
		return err
	}
	return nil
}

// Sync fetches the branch and anchor tags (both no-force), verifies the
// newest anchor is an ancestor of the head, and returns the head SHA.
func (s *Store) Sync() (string, error) {
	if err := s.fetch(); err != nil {
		return "", err
	}
	head, ok := s.Repo.ResolveRef(localRef)
	if !ok {
		return "", fmt.Errorf("state ref missing after fetch")
	}
	if err := s.Repo.FetchTagsNoForce(s.Remote, anchorGlob); err != nil {
		var ff *gitx.ErrNonFastForward
		if errors.As(err, &ff) {
			return "", &IntegrityError{Reason: "anchor_moved", Detail: "a seed-anchor tag changed on the remote; refusing to proceed."}
		}
		return "", err
	}
	tags, err := s.Repo.ListTags(anchorGlob)
	if err != nil {
		return "", err
	}
	if len(tags) > 0 {
		sort.Strings(tags) // timestamps sort lexically
		newest := tags[len(tags)-1]
		anc, err := s.Repo.IsAncestor(newest+"^{commit}", head)
		if err != nil {
			return "", err
		}
		if !anc {
			return "", &IntegrityError{Reason: "anchor_ancestry_failed", Detail: fmt.Sprintf("anchor %s is not an ancestor of the fetched head — rewrite detected (fresh clones included, §7.2)", newest)}
		}
	}
	return head, nil
}

// Halted reports whether the HALT marker is present at head, with its content.
func (s *Store) Halted(head string) (bool, string) {
	content, ok, err := s.Repo.CatFile(head, haltPath)
	if err != nil || !ok {
		return false, ""
	}
	return true, strings.TrimSpace(content)
}

func (s *Store) ReadFile(head, path string) (string, bool, error) {
	return s.Repo.CatFile(head, path)
}

func (s *Store) ListDir(head, dir string) ([]string, error) {
	return s.Repo.ListTree(head, dir)
}

// Mutation is what one verb wants committed: a message, file changes, and
// run-log event lines (appended atomically in the same commit, §7.2).
type Mutation struct {
	Message string
	Changes []gitx.Change
	Events  []string // JSON lines
}

// Mutate runs the fetch→build→commit→push loop with jittered backoff. build
// re-reads state at the given head each attempt and may return a *Terminal to
// refuse without retry (contention, fencing, invalid transitions decide on
// fresh state). checkHalt guards mutating verbs; `seed state resume` passes
// false to clear the marker.
func (s *Store) Mutate(checkHalt bool, build func(head string) (*Mutation, error)) (string, error) {
	var lastErr error
	for attempt := 0; attempt < s.MaxAttempts; attempt++ {
		head, err := s.Sync()
		if err != nil {
			return "", err
		}
		if checkHalt {
			if halted, reason := s.Halted(head); halted {
				return "", &IntegrityError{Reason: "halted", Detail: "state ref carries a HALT marker (" + reason + "); mutating verbs refused until `seed state resume` (§7.2)"}
			}
		}
		mut, err := build(head)
		if err != nil {
			return "", err
		}
		changes := mut.Changes
		if len(mut.Events) > 0 {
			log, _, err := s.ReadFile(head, runLogPath)
			if err != nil {
				return "", err
			}
			if log != "" && !strings.HasSuffix(log, "\n") {
				log += "\n"
			}
			changes = append(changes, gitx.Change{Path: runLogPath, Content: log + strings.Join(mut.Events, "\n") + "\n"})
		}
		commit, err := s.Repo.CommitTree(head, []string{head}, mut.Message, changes)
		if err != nil {
			return "", err
		}
		err = s.Repo.Push(s.Remote, commit, s.Branch)
		if err == nil {
			if _, err := s.Repo.Git("update-ref", localRef, commit); err != nil {
				return "", err
			}
			return commit, nil
		}
		var ff *gitx.ErrNonFastForward
		if !errors.As(err, &ff) {
			return "", err
		}
		lastErr = err
		s.Sleep(backoff(attempt))
	}
	return "", fmt.Errorf("state-ref contention: push rejected %d times (R4): %w", s.MaxAttempts, lastErr)
}

func backoff(attempt int) time.Duration {
	base := time.Duration(1<<attempt) * 100 * time.Millisecond
	return base + time.Duration(rand.Int63n(int64(base)))
}
