package task

import (
	"fmt"
	"path"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/card"
	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

// Continuation packets (plan os-499c5978; inspirations/08's packet,
// mechanical-first, bounded, disclosed). One renderer serves both paths:
// the worker-invoked generator (`seed handoff generate`), whose checkout
// IS the workspace, and the write_handoff transition effect
// (release/park/reap). Workspace anchors are only meaningful when the
// generating process runs in the worker's checkout — a reap runs in the
// maintenance checkout, so its packet marks the anchors unavailable
// instead of recording the reaper's unrelated git state.
const handoffBound = 8 << 10

type wsAnchors struct {
	branch, sha string
	dirty       []string
}

func (sv *Service) collectAnchors() *wsAnchors {
	repo := &gitx.Repo{Dir: sv.Root}
	branch, _ := repo.Git("rev-parse", "--abbrev-ref", "HEAD")
	sha, _ := repo.Git("rev-parse", "--short", "HEAD")
	status, _ := repo.Git("status", "--porcelain")
	a := &wsAnchors{branch: branch, sha: sha}
	for _, line := range strings.Split(status, "\n") {
		if len(line) > 3 {
			a.dirty = append(a.dirty, strings.TrimSpace(line[3:]))
		}
	}
	return a
}

// renderPacket builds the bounded packet. anchors == nil means the
// workspace was not observable (reap): say so rather than guessing.
func (sv *Service) renderPacket(c *card.Card, reason string, prior *card.Claim, anchors *wsAnchors, blockedOnExtra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Continuation packet — %s\n", c.ID)
	fmt.Fprintf(&b, "> Generated %s by seed handoff (reason: %s; mechanical-first: card + git; no prose was invented).\n", sv.now(), reason)
	fmt.Fprintf(&b, "> Read this before acting; your session has no memory of prior turns.\n\n")
	fmt.Fprintf(&b, "## Task\n%s — state %s, priority %s.\nThe card body is the work order (re-read it: `seed task get %s`).\n\n", c.Title, c.State, c.Priority, c.ID)
	if prior != nil {
		fmt.Fprintf(&b, "## Claim\nHeld by %s, lease expires %s. A reaped claim's token is dead (exit 6) — reclaim before working.\n\n", prior.Actor, prior.LeaseExpires)
	}
	if len(c.BlockedOn) > 0 || blockedOnExtra != "" {
		entries := c.BlockedOn
		if blockedOnExtra != "" && !strings.Contains(strings.Join(entries, ","), blockedOnExtra) {
			entries = append(append([]string{}, entries...), blockedOnExtra)
		}
		fmt.Fprintf(&b, "## Blocked on\n%s\n", strings.Join(entries, ", "))
		if strings.HasPrefix(blockedOnExtra, "plan:") {
			fmt.Fprintf(&b, "salvageable: true\n")
		}
		b.WriteString("\n")
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
	if anchors == nil {
		fmt.Fprintf(&b, "## Workspace anchor\nunavailable — claim reaped by maintenance; the worker's workspace state was not observable. Expect branch seed/%s if work began.\n", c.ID)
	} else {
		fmt.Fprintf(&b, "## Workspace anchor\nbranch %s @ %s", anchors.branch, anchors.sha)
		if len(anchors.dirty) > 0 {
			fmt.Fprintf(&b, " · %d file(s) uncommitted: %s", len(anchors.dirty), strings.Join(anchors.dirty, ", "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n## Next step\nRe-read the card and plan; run `make check` before pushing.\n")

	content := b.String()
	if len(content) > handoffBound {
		content = content[:handoffBound-64] + "\n\n[truncated at 8KB — the card is the full record]\n"
	}
	return content
}

// HandoffGenerate renders the continuation packet for a card. With
// write=true the packet lands at handoff/<task-id>.md on the state ref
// (the same location the v1 reap stub used — this generator supersedes
// it). The caller's checkout is the workspace, so anchors are observable.
func (sv *Service) HandoffGenerate(id, actor string, write bool) *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	c, err := sv.loadCard(head, id)
	if err != nil {
		return errResult(err)
	}
	content := sv.renderPacket(c, "handoff-generate", c.Claim, sv.collectAnchors(), "")
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
