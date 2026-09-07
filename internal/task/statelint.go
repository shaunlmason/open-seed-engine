// State-ref conformance lint (§7.2/D7): card structural lint, done-
// consistency, and commit-over-commit replay of the state ref against the
// transition table. A failure with --halt-on-fail writes the HALT marker:
// the shim then refuses mutating verbs until a human runs `seed state
// resume`. This catches semantic (fast-forward) tampering that survives the
// push protections; its trust is push-access-deep like everything on the ref
// (R10).
package task

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/card"
	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

const replayLimit = 1000

// StateLint runs all state-ref lints. With no state ref yet it reports ok:
// check+validate runs on repos that have not run `seed init`.
func (sv *Service) StateLint(haltOnFail bool, actor string) *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		var noRef *gitx.ErrNoRemoteRef
		if errors.As(err, &noRef) {
			return ok(map[string]any{"verb": "state-lint", "note": "no state ref yet (run seed init)"})
		}
		return errResult(err)
	}

	var failures []string
	failures = append(failures, sv.lintCards(head)...)
	if _, isGit := sv.Store.(*stateref.Store); isGit {
		failures = append(failures, sv.replay(head)...)
	}
	// Non-git stores have no commit history: the replay lint does not apply
	// (declared variance: machine-local trust), the card lints still do.

	if len(failures) == 0 {
		return ok(map[string]any{"verb": "state-lint", "head": head})
	}
	if haltOnFail {
		if halted, _ := sv.Store.Halted(head); !halted {
			_, herr := sv.Store.Mutate(false, func(h string) (*stateref.Mutation, error) {
				return &stateref.Mutation{
					Message: "state lint: conformance failure, writing HALT",
					Changes: []gitx.Change{{Path: "HALT", Content: "conformance lint failed:\n- " + strings.Join(failures, "\n- ") + "\n"}},
					Events:  []string{sv.event(actor, "halt", "", map[string]any{"failures": len(failures)})},
				}, nil
			})
			if herr != nil {
				failures = append(failures, "additionally, writing HALT failed: "+herr.Error())
			}
		}
	}
	return &Result{Code: 1, Err: "conformance_failed", Fields: map[string]any{"verb": "state-lint", "failures": failures, "halted": haltOnFail}}
}

// lintCards: structural card lint + done-consistency (D7).
func (sv *Service) lintCards(head string) []string {
	var failures []string
	names, err := sv.Store.ListDir(head, "tasks")
	if err != nil {
		return []string{"list tasks: " + err.Error()}
	}
	for _, n := range names {
		if !strings.HasSuffix(n, ".md") {
			continue
		}
		content, found, err := sv.Store.ReadFile(head, "tasks/"+n)
		if err != nil || !found {
			failures = append(failures, "unreadable card "+n)
			continue
		}
		c, err := card.Parse(content)
		if err != nil {
			failures = append(failures, "malformed card "+n+": "+err.Error())
			continue
		}
		validState := slices.Contains(sv.Spec.Port.States, c.State)
		if !validState {
			failures = append(failures, c.ID+": unknown state "+c.State)
			continue
		}
		claimBearing := slices.Contains(sv.Spec.Port.ClaimBearingStates, c.State)
		if claimBearing && c.Claim == nil {
			failures = append(failures, c.ID+": in_progress without a claim block")
		}
		if !claimBearing && c.Claim != nil {
			failures = append(failures, c.ID+": state "+c.State+" carries a claim — no state other than in_progress ever does (§7.1)")
		}
		if c.State == "blocked" && len(c.BlockedOn) == 0 {
			failures = append(failures, c.ID+": blocked with empty blocked_on")
		}
		if c.State == "done" {
			failures = append(failures, sv.lintDone(c)...)
		}
	}
	return failures
}

// lintDone is the done-consistency lint (D7): every done card corresponds to
// reviewed, evidenced work, or carries the no-PR exemption, whose evidence
// is the server-attributed artifact. Reviewer identity must resolve to the
// operator roster; the workflow-side lint additionally checks the server's
// own attribution (this local check is roster-shaped, stated per R10).
func (sv *Service) lintDone(c *card.Card) []string {
	var failures []string
	if c.Review == nil || c.Review.Outcome != "accepted" {
		failures = append(failures, c.ID+": done without an accepted review block")
		return failures
	}
	if c.Review.Evidence == "" {
		failures = append(failures, c.ID+": done without evidence (merged PR URL or no-PR artifact, D4.5/D7)")
	}
	if !sv.Cfg.IsOperator(c.Review.Reviewer) {
		failures = append(failures, c.ID+": done accepted by "+c.Review.Reviewer+", not in the operator roster — forged accept (D7)")
	}
	if !strings.HasPrefix(c.Review.Evidence, "no-pr:") && c.Review.PlanExempt == "" && !sv.planResolves(c.ID) {
		failures = append(failures, c.ID+": done without a resolvable plan at plans/"+c.ID+".md, without the no-PR exemption and without an operator plan exemption (D3/D7)")
	}
	return failures
}

// planResolves answers whether this card has an approved plan, and asks
// the DEFAULT BRANCH rather than the checkout. The working tree is a
// per-clone, per-branch fact: an agent sitting on a branch cut before a
// plan merged would otherwise fail the lint for every card planned
// since, and with --halt-on-fail one stale clone halts the ref for
// everyone. The plan lives in the code repository at Root whatever
// backend holds the cards, so the probe is the repository's, not the
// store's.
//
// It asks whether the path EXISTS on the default branch now, not
// whether some commit once touched it: a plan on an unmerged branch has
// not passed its own PR gate, and counting it would let an unplanned
// card satisfy D3. A clone that has not fetched since the plan merged
// still answers no, and the halt is then the operator's to resume.
func (sv *Service) planResolves(id string) bool {
	if _, err := os.Stat(filepath.Join(sv.Root, filepath.FromSlash("plans/"+id+".md"))); err == nil {
		return true
	}
	return sv.planApproved(id)
}

// planApproved is the same question with the checkout's own answer left
// out: does an APPROVED plan exist, on a branch that has passed its own
// PR gate. The accept edge's plan gate reads this rather than
// planResolves, and the difference is load-bearing. The lint re-runs, so
// reading a plan that is merely present in this checkout costs a later
// re-read at worst. The gate does not: it decides whether a card may
// become terminal, and `done` is terminal, so a plan that exists only on
// the accepting worktree would let the card through, vanish on the next
// checkout, and leave exactly the permanently lint-failing card the gate
// exists to prevent (review finding on #15).
//
// A clone that has not fetched since the plan merged answers no, and the
// accept then refuses until it fetches. That is the safe direction: the
// cost is one fetch, against a card nothing can repair.
func (sv *Service) planApproved(id string) bool {
	rel := "plans/" + id + ".md"
	repo := &gitx.Repo{Dir: sv.Root}
	for _, ref := range defaultBranchRefs(repo) {
		if _, err := repo.Git("cat-file", "-e", ref+":"+rel); err == nil {
			return true
		}
	}
	return false
}

// defaultBranchRefs names the refs that can carry an APPROVED plan, in
// order: the remote's own default branch, then the conventional names.
// HEAD and feature branches are deliberately absent, since a plan that
// has not reached the default branch has not passed its plan PR.
func defaultBranchRefs(repo *gitx.Repo) []string {
	var refs []string
	if out, err := repo.Git("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if named := strings.TrimSpace(out); named != "" {
			refs = append(refs, named)
		}
	}
	return append(refs, "origin/main", "origin/master", "main", "master")
}

// replay walks the state ref's history (bounded) and checks every card state
// change against the transition table, and that mutating commits append to
// the run log: hand-editing the ref cannot produce a legal-looking history.
func (sv *Service) replay(head string) []string {
	gitStore, isGit := sv.Store.(*stateref.Store)
	if !isGit {
		return nil // no commit history to replay (guarded by the caller too)
	}
	repo := gitStore.Repo
	out, err := repo.Git("rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", replayLimit), head)
	if err != nil {
		return []string{"rev-list: " + err.Error()}
	}
	commits := strings.Split(strings.TrimSpace(out), "\n")
	var failures []string
	for i := 0; i < len(commits)-1; i++ {
		child, parent := commits[i], commits[i+1]
		filesOut, err := repo.Git("diff", "--name-only", parent, child)
		if err != nil {
			failures = append(failures, "diff "+child[:12]+": "+err.Error())
			continue
		}
		files := strings.Split(strings.TrimSpace(filesOut), "\n")
		cardChanged := false
		runlogChanged := false
		for _, f := range files {
			switch {
			case f == "run-log.jsonl":
				runlogChanged = true
			case strings.HasPrefix(f, "tasks/") && strings.HasSuffix(f, ".md"):
				cardChanged = true
				if fail := sv.checkCardChange(parent, child, f); fail != "" {
					failures = append(failures, fail)
				}
			}
		}
		if cardChanged && !runlogChanged {
			failures = append(failures, "commit "+child[:12]+" mutates cards without appending to run-log.jsonl (§7.2 atomicity)")
		}
	}
	return failures
}

func (sv *Service) checkCardChange(parent, child, path string) string {
	oldC, oldOK, _ := sv.Store.ReadFile(parent, path)
	newC, newOK, _ := sv.Store.ReadFile(child, path)
	if !newOK {
		return "" // deletion: truncation is a documented human operation
	}
	nc, err := card.Parse(newC)
	if err != nil {
		return path + " unparseable at " + child[:12]
	}
	if !oldOK {
		if nc.State != "backlog" {
			return nc.ID + ": created in state " + nc.State + " (cards are born in backlog)"
		}
		return ""
	}
	oc, err := card.Parse(oldC)
	if err != nil {
		return "" // older malformed card; the structural lint reports it
	}
	if oc.State == nc.State {
		return ""
	}
	for _, t := range sv.Spec.Table.Transitions {
		if t.From == oc.State && t.To == nc.State {
			return ""
		}
	}
	return fmt.Sprintf("%s: illegal transition %s→%s at %.12s (not in the D1 table)", nc.ID, oc.State, nc.State, child)
}
