package task

// The accept edge's plan gate (plans/os-c4f25e13.md D1): the D3 plan gate
// and the D7 exemption are alternatives, and one must hold at the moment a
// card becomes terminal. These drills pin the door. The repair behind it
// (exempt-plan, record-evidence) and the lint that catches what got through
// before the door existed are drilled in exemptplan_test.go and
// maintain_test.go; TestLintStillRefusesAPlanlessDoneCard below pins that
// the lint's own rule did not move when the door was added.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reviewReady drives one card to review and returns its id.
func reviewReady(t *testing.T, sv *Service, title string) string {
	t.Helper()
	id := createReady(t, sv, title)
	r := mustOK(t, sv.Claim(id, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)
	mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review",
		Actor: "agent-a", Token: tok}))
	return id
}

// TestAcceptRefusesPlanlessCloseWithoutExemption pins the prevention half.
// Before this gate a card with no plan closed on a bare resolution, landed
// in done, and failed the conformance lint from then on; done is terminal,
// so the remedies were all out of band and all paid under a halt, because a
// failing lint refuses every mutating verb for every actor. The refusal
// belongs where the card is still in review and still fixable.
func TestAcceptRefusesPlanlessCloseWithoutExemption(t *testing.T) {
	for _, verb := range []string{"accept", "close"} {
		t.Run(verb, func(t *testing.T) {
			sv := fastService(t, "")
			mustOK(t, sv.Init())

			// No plan, no exemption: refused, and the card is untouched.
			id := reviewReady(t, sv, "planless")
			got := sv.Transition(TransitionArgs{Verb: verb, ID: id, Actor: "lead",
				Resolution: "https://example.invalid/pr/1"})
			if got.Code == 0 || got.Err != "plan_required" {
				t.Fatalf("a planless %s must refuse: code=%d err=%s", verb, got.Code, got.Err)
			}
			detail, _ := got.Fields["detail"].(string)
			for _, want := range []string{"plans/<id>.md", "--no-pr"} {
				if !strings.Contains(detail, want) {
					t.Fatalf("the refusal must name %q: %q", want, detail)
				}
			}
			if c := getCard(t, sv, id); c.State != "review" || c.Review != nil {
				t.Fatalf("a refused %s changes nothing: state=%s review=%+v", verb, c.State, c.Review)
			}

			// The exemption satisfies it, and lands the marker the lint reads.
			mustOK(t, sv.Transition(TransitionArgs{Verb: verb, ID: id, Actor: "lead", NoPR: true,
				Resolution: "https://example.invalid/run/1"}))
			c := getCard(t, sv, id)
			if c.State != "done" || !strings.HasPrefix(c.Review.Evidence, "no-pr:") {
				t.Fatalf("the exemption lands the marker: state=%s review=%+v", c.State, c.Review)
			}

			// A plan satisfies it with no flag, and the evidence stays bare.
			planned := reviewReady(t, sv, "planned")
			writePlan(t, sv, planned)
			mustOK(t, sv.Transition(TransitionArgs{Verb: verb, ID: planned, Actor: "lead",
				Resolution: "https://example.invalid/pr/2"}))
			if c := getCard(t, sv, planned); c.State != "done" || c.Review.Evidence != "https://example.invalid/pr/2" {
				t.Fatalf("a planned card closes unchanged: state=%s review=%+v", c.State, c.Review)
			}

			// Both rules unmet: the evidence refusal is the one reported,
			// so the caller fixes the first thing wrong rather than
			// learning about the gate only after supplying a resolution.
			both := reviewReady(t, sv, "neither")
			if got := sv.Transition(TransitionArgs{Verb: verb, ID: both, Actor: "lead"}); got.Err != "resolution_required" {
				t.Fatalf("with neither rule met the evidence refusal reports first, got %s", got.Err)
			}
		})
	}
}

// TestGateRefusesAPlanOnlyInTheWorktree pins the review finding on #15.
// planResolves answers the lint, and takes the checkout's word: for a
// re-runnable check that costs a re-read at worst. The gate cannot take
// it. A plan written on the accepting branch and never merged would let
// the card go terminal, and the file then vanishes on the next checkout,
// leaving exactly the permanently lint-failing card the gate exists to
// prevent. The gate therefore reads the default-branch refs alone.
func TestGateRefusesAPlanOnlyInTheWorktree(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	id := reviewReady(t, sv, "plan in the worktree only")

	// Written, not committed: the lint's helper says yes, the gate says no.
	plans := filepath.Join(sv.Root, "plans")
	if err := os.MkdirAll(plans, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plans, id+".md"), []byte("# unmerged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sv.planResolves(id) {
		t.Fatal("the lint's helper takes the checkout's word")
	}
	if sv.planApproved(id) {
		t.Fatal("an uncommitted plan has passed no PR gate and must not count as approved")
	}
	got := sv.Transition(TransitionArgs{Verb: "close", ID: id, Actor: "lead",
		Resolution: "https://example.invalid/pr/1"})
	if got.Code == 0 || got.Err != "plan_required" {
		t.Fatalf("a worktree-only plan must not satisfy the gate: code=%d err=%s", got.Code, got.Err)
	}

	// Landed on the default branch, it does.
	mustGit(t, sv.Root, "add", "--", "plans/"+id+".md")
	mustGit(t, sv.Root, "-c", "user.email=t@example.invalid", "-c", "user.name=t",
		"commit", "-m", "land the plan")
	if !sv.planApproved(id) {
		t.Fatal("a plan on the default branch is approved")
	}
	mustOK(t, sv.Transition(TransitionArgs{Verb: "close", ID: id, Actor: "lead",
		Resolution: "https://example.invalid/pr/1"}))
}

// TestLintStillRefusesAPlanlessDoneCard pins that the door did not move the
// lint. The conformance rule stays three-way: a resolvable plan, the no-pr:
// evidence marker, or an operator's recorded plan exemption. Softening it
// into accepting any merged-PR evidence would let a card that genuinely
// skipped a required plan satisfy D3, which is the whole point of the check.
func TestLintStillRefusesAPlanlessDoneCard(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())

	// Plan-less and marker-less: the state the door now prevents and the
	// lint still catches, built the way a store predating the door holds it.
	bad := reviewReady(t, sv, "legacy planless")
	closePlanless(t, sv, bad, "https://example.invalid/pr/1")
	if !lintFails(t, sv, bad) {
		t.Fatal("a done card with neither a plan nor the exemption must still fail the lint")
	}

	// The marker satisfies it.
	marked := reviewReady(t, sv, "exempt")
	mustOK(t, sv.Transition(TransitionArgs{Verb: "close", ID: marked, Actor: "lead", NoPR: true,
		Resolution: "https://example.invalid/run/1"}))
	if lintFails(t, sv, marked) {
		t.Fatal("the no-pr marker must satisfy the lint")
	}

	// A resolvable plan satisfies it.
	planned := reviewReady(t, sv, "planned")
	writePlan(t, sv, planned)
	mustOK(t, sv.Transition(TransitionArgs{Verb: "close", ID: planned, Actor: "lead",
		Resolution: "https://example.invalid/pr/2"}))
	if lintFails(t, sv, planned) {
		t.Fatal("a resolvable plan must satisfy the lint")
	}

	// And the operator's recorded exemption still repairs the legacy card,
	// which is what the door leaves behind rather than replaces.
	mustOK(t, sv.ExemptPlan(bad, "lead", "the deliverable was itself a plan PR"))
	if lintFails(t, sv, bad) {
		t.Fatal("a recorded plan exemption must clear the lint")
	}
}
