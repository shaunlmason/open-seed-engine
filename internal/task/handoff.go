package task

import (
	"fmt"
	"path"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

// HandoffGenerate renders the continuation packet for a card
// (plan os-499c5978; inspirations/08's packet, mechanical-first,
// bounded, disclosed): card goal/criteria + claim block from the state
// ref, branch/HEAD/dirty-file anchors from git. With write=true the
// packet lands at handoff/<task-id>.md on the state ref (the same
// location the v1 reap stub used — this generator supersedes it).
const handoffBound = 8 << 10

func (sv *Service) HandoffGenerate(id, actor string, write bool) *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	c, err := sv.loadCard(head, id)
	if err != nil {
		return errResult(err)
	}

	repo := &gitx.Repo{Dir: sv.Root}
	branch, _ := repo.Git("rev-parse", "--abbrev-ref", "HEAD")
	sha, _ := repo.Git("rev-parse", "--short", "HEAD")
	status, _ := repo.Git("status", "--porcelain")
	var dirty []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) > 3 {
			dirty = append(dirty, strings.TrimSpace(line[3:]))
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Continuation packet — %s\n", id)
	fmt.Fprintf(&b, "> Generated %s by seed handoff (mechanical-first: card + git; no prose was invented).\n", sv.now())
	fmt.Fprintf(&b, "> Read this before acting; your session has no memory of prior turns.\n\n")
	fmt.Fprintf(&b, "## Task\n%s — state %s, priority %s.\nThe card body is the work order (re-read it: `seed task get %s`).\n\n", c.Title, c.State, c.Priority, id)
	if c.Claim != nil {
		fmt.Fprintf(&b, "## Claim\nHeld by %s, lease expires %s. A reaped claim's token is dead (exit 6) — reclaim before working.\n\n", c.Claim.Actor, c.Claim.LeaseExpires)
	}
	if len(c.BlockedOn) > 0 {
		fmt.Fprintf(&b, "## Blocked on\n%s\n\n", strings.Join(c.BlockedOn, ", "))
	}
	// Evidence trail: the card body's appended sections carry the record.
	var evidence []string
	for _, line := range strings.Split(c.Body, "\n") {
		if strings.HasPrefix(line, "## Evidence") || strings.HasPrefix(line, "## Comment") {
			evidence = append(evidence, strings.TrimPrefix(line, "## "))
		}
	}
	if len(evidence) > 0 {
		fmt.Fprintf(&b, "## Recorded trail\n- %s\n\n", strings.Join(evidence, "\n- "))
	}
	fmt.Fprintf(&b, "## Workspace anchor\nbranch %s @ %s", branch, sha)
	if len(dirty) > 0 {
		fmt.Fprintf(&b, " · %d file(s) uncommitted: %s", len(dirty), strings.Join(dirty, ", "))
	}
	fmt.Fprintf(&b, "\n\n## Next step\nRe-read the card and plan; run `make check` before pushing.\n")

	content := b.String()
	if len(content) > handoffBound {
		content = content[:handoffBound-64] + "\n\n[truncated at 8KB — the card is the full record]\n"
	}
	if !write {
		return ok(map[string]any{"verb": "handoff-generate", "task": id, "packet": content})
	}
	if actor == "" {
		actor = "seed"
	}
	_, err = sv.Store.Mutate(true, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{
			Message: "handoff " + id,
			Changes: []gitx.Change{{Path: path.Join("handoff", id+".md"), Content: content}},
			Events:  []string{sv.event(actor, "handoff-generate", id, nil)},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "handoff-generate", "task": id, "written": "handoff/" + id + ".md"})
}
