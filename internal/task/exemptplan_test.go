package task

// The plan exemption and the plan resolver (D3/D7). Two failures put this
// verb here, both observed on a live queue. First, `done` is terminal, so a
// card accepted without a plan and without the no-PR exemption could be
// repaired by no transition at all: the lint refused it forever and the
// only remedy was rewriting the store. Second, the plan check asked the
// WORKING TREE, so an agent on a branch cut before a plan merged failed the
// lint for every card planned since, and with --halt-on-fail one stale
// clone halted the ref for everyone.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doneWithoutAPlan is an accepted card carrying PR evidence and no plan
// file: exactly the shape the done lint refuses.
func doneWithoutAPlan(t *testing.T, sv *Service) string {
	t.Helper()
	return acceptedCard(t, sv, "https://example.invalid/pr/1")
}

func lintFails(t *testing.T, sv *Service, id string) bool {
	t.Helper()
	r := sv.StateLint(false, "lead")
	if r.Code == 0 {
		return false
	}
	for _, f := range r.Fields["failures"].([]string) {
		if strings.HasPrefix(f, id+":") && strings.Contains(f, "plan") {
			return true
		}
	}
	return false
}

// The repair the terminal state forbids: the lint refuses the card, the
// operator states why no plan was owed, and the same card passes, with no
// transition and no rewrite of the store.
func TestExemptPlanClearsTheDoneLint(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	id := doneWithoutAPlan(t, sv)
	if !lintFails(t, sv, id) {
		t.Fatal("a done card with no plan must fail the lint before the exemption")
	}
	mustOK(t, sv.ExemptPlan(id, "lead", "its whole deliverable was another card's plan"))
	if lintFails(t, sv, id) {
		t.Fatal("the recorded exemption must clear the plan failure")
	}
	if got := getCard(t, sv, id).Review.PlanExempt; got != "its whole deliverable was another card's plan" {
		t.Fatalf("the reason is kept verbatim on the card: %q", got)
	}
}

// The exemption is an operator judgment that has to be justified, on a card
// that was actually reviewed, and it is not a field an implementer can set
// or an operator can quietly rewrite.
func TestExemptPlanRefusals(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	id := doneWithoutAPlan(t, sv)

	if r := sv.ExemptPlan(id, "agent-a", "because I say so"); r.Code == 0 || r.Err != "operator_required" {
		t.Fatalf("a worker cannot exempt its own card: (%d,%s)", r.Code, r.Err)
	}
	if r := sv.ExemptPlan(id, "lead", "   "); r.Code == 0 || r.Err != "reason_required" {
		t.Fatalf("an exemption nobody justifies erases the rule: (%d,%s)", r.Code, r.Err)
	}
	open := createReady(t, sv, "never accepted")
	if r := sv.ExemptPlan(open, "lead", "no plan owed"); r.Code == 0 || r.Err != "no_accepted_review" {
		t.Fatalf("a card nobody accepted has nothing to exempt: (%d,%s)", r.Code, r.Err)
	}
	mustOK(t, sv.ExemptPlan(id, "lead", "L1, no plan owed"))
	if r := sv.ExemptPlan(id, "lead", "a different story"); r.Code == 0 || r.Err != "plan_exemption_already_recorded" {
		t.Fatalf("a recorded exemption is not rewritable: (%d,%s)", r.Code, r.Err)
	}
	if got := getCard(t, sv, id).Review.PlanExempt; got != "L1, no plan owed" {
		t.Fatalf("the first reason stands: %q", got)
	}
}

// The stale-checkout case, which is the one that halted a live ref: the
// plan is committed in the repository but absent from the working tree,
// as it is for every agent on a branch cut before the plan merged. The
// lint must resolve it from git and pass.
func TestPlanResolvesFromGitNotTheCheckout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	mustGit(t, "", "init", "--initial-branch=main", dir)
	sv := fastService(t, dir)
	mustOK(t, sv.Init())
	id := doneWithoutAPlan(t, sv)
	if !lintFails(t, sv, id) {
		t.Fatal("no plan anywhere must fail")
	}

	// Commit the plan, then take it out of the working tree: committed
	// history says the card was planned, the checkout does not.
	plans := filepath.Join(dir, "plans")
	if err := os.MkdirAll(plans, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(plans, id+".md")
	if err := os.WriteFile(path, []byte("# plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "plans")
	mustGit(t, dir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", "plan")
	if lintFails(t, sv, id) {
		t.Fatal("a plan in the working tree must pass")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if lintFails(t, sv, id) {
		t.Fatal("a plan committed but absent from this checkout must still resolve: a stale clone must never halt the shared ref")
	}
}

// The other half of asking git: only the DEFAULT BRANCH counts. A plan
// sitting on an unmerged branch has not passed its own plan PR, so a
// card whose plan never merged must still fail D3. A resolver that
// accepted any ref (git rev-list --all) passes this card and lets an
// unplanned card through, which is the loophole the narrower query
// closes.
func TestPlanOnAnUnmergedBranchDoesNotSatisfyD3(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	mustGit(t, "", "init", "--initial-branch=main", dir)
	sv := fastService(t, dir)
	mustOK(t, sv.Init())
	id := doneWithoutAPlan(t, sv)

	// main needs a commit before a branch can leave it.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := func(msg string) {
		t.Helper()
		mustGit(t, dir, "add", "-A")
		mustGit(t, dir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-m", msg)
	}
	commit("base")

	mustGit(t, dir, "checkout", "-q", "-b", "seed/"+id+"-plan")
	plans := filepath.Join(dir, "plans")
	if err := os.MkdirAll(plans, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plans, id+".md"), []byte("# plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit("plan on a branch nobody merged")
	mustGit(t, dir, "checkout", "-q", "main")

	if !lintFails(t, sv, id) {
		t.Fatal("a plan that never reached the default branch must not satisfy D3")
	}
}
