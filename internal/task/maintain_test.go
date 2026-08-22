package task

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

// Done-when (reap path): maintain reap releases exactly the expired leases,
// writes handoff stubs, and leaves live claims alone.
func TestMaintainReap(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	expiredID := createReady(t, a, "expired work")
	liveID := createReady(t, a, "live work")
	mustOK(t, a.Claim(expiredID, "agent-a", "1m"))
	mustOK(t, a.Claim(liveID, "agent-b", "24h"))

	later := time.Now().Add(2 * time.Hour)
	a.Now = func() time.Time { return later }
	r := mustOK(t, a.ReapExpired("lead"))
	reaped, _ := r.Fields["reaped"].([]string)
	if len(reaped) != 1 || reaped[0] != expiredID {
		t.Fatalf("reaped = %v", r.Fields)
	}
	if g := mustOK(t, a.Get(expiredID)); g.Fields["state"] != "ready" {
		t.Fatalf("expired card state = %v", g.Fields["state"])
	}
	if g := mustOK(t, a.Get(liveID)); g.Fields["state"] != "in_progress" {
		t.Fatalf("live card state = %v", g.Fields["state"])
	}
	head, _ := a.Store.Sync()
	if _, found, _ := a.Store.ReadFile(head, "handoff/"+expiredID+".md"); !found {
		t.Fatal("reap wrote no handoff stub")
	}
}

// Done-when (close path, plan side): the state-shaped plan-unblock removes
// only its own entry and transitions only when the set empties.
func TestPlanUnblock(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	blocker := createReady(t, a, "the blocker")
	// The planned card depends on the blocker from birth.
	r := mustOK(t, a.Create(CreateArgs{Title: "planned work", Actor: "lead", BlockedBy: []string{blocker}}))
	id := r.Fields["task"].(string)
	mustOK(t, a.Transition(TransitionArgs{Verb: "promote", ID: id, Actor: "lead"}))
	cl := mustOK(t, a.Claim(id, "planner-1", ""))
	tok := cl.Fields["claim_token"].(string)
	// PR-first, then park: blocked_on now [dep:<blocker>, plan:41].
	mustOK(t, a.Transition(TransitionArgs{Verb: "transition", ID: id, To: "blocked",
		Actor: "planner-1", Token: tok, BlockedOn: "plan:41"}))

	if r := a.PlanUnblock(id, 41, "not-an-operator"); r.Code != spec.ExitInvalid {
		t.Fatalf("non-operator plan-unblock: %+v", r)
	}
	pu := mustOK(t, a.PlanUnblock(id, 41, "lead"))
	if pu.Fields["state"] != "blocked" {
		t.Fatalf("removing plan entry must leave the dep-blocked card blocked: %v", pu.Fields)
	}
	if r := a.PlanUnblock(id, 41, "lead"); r.Code == 0 {
		t.Fatal("double plan-unblock succeeded")
	}
	// Resolving the dep via the blocker's close empties the set → ready.
	bc := mustOK(t, a.Claim(blocker, "agent-b", ""))
	btok := bc.Fields["claim_token"].(string)
	mustOK(t, a.Transition(TransitionArgs{Verb: "transition", ID: blocker, To: "review", Actor: "agent-b", Token: btok}))
	closed := mustOK(t, a.Transition(TransitionArgs{Verb: "close", ID: blocker, Actor: "lead",
		Resolution: "https://example.com/run/1", NoPR: true}))
	cascaded, _ := closed.Fields["cascaded"].([]string)
	if len(cascaded) != 1 || cascaded[0] != id {
		t.Fatalf("cascade = %v", closed.Fields)
	}
	if g := mustOK(t, a.Get(id)); g.Fields["state"] != "ready" {
		t.Fatalf("card state = %v", g.Fields["state"])
	}
}

// The no-PR close records the exemption marker, satisfying done-consistency
// without a plan file (D7).
func TestNoPRCloseAndDoneConsistency(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "no-pr work")
	r := mustOK(t, a.Claim(id, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)
	mustOK(t, a.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-a", Token: tok}))
	mustOK(t, a.Transition(TransitionArgs{Verb: "close", ID: id, Actor: "lead",
		Resolution: "https://github.com/x/y/actions/runs/1", NoPR: true}))

	lint := a.StateLint(false, "maintenance")
	if lint.Code != 0 {
		t.Fatalf("no-pr close failed done-consistency: %v", lint.Fields)
	}
}

// A done card closed without the exemption and without a plan file fails the
// done-consistency lint.
func TestDoneConsistencyRequiresPlanOrExemption(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "planless done")
	r := mustOK(t, a.Claim(id, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)
	mustOK(t, a.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-a", Token: tok}))
	mustOK(t, a.Transition(TransitionArgs{Verb: "close", ID: id, Actor: "lead", Resolution: "PR merged (allegedly)"}))

	lint := a.StateLint(false, "maintenance")
	if lint.Code == 0 {
		t.Fatal("done without plan or exemption passed lint")
	}
	if !lintFailuresContain(lint, "without a resolvable plan") {
		t.Fatalf("failures = %v", lint.Fields["failures"])
	}
}

// Done-when (HALT path): hand-forged illegal state on the ref is caught by
// the replay lint; --halt-on-fail writes HALT; mutating verbs refuse until
// operator resume.
func TestStateLintCatchesForgedTransitionAndHalts(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "forged card")

	// Attacker with push access hand-edits the card straight to done —
	// bypassing the shim (fast-forward, so push protections don't fire).
	head, err := a.Store.Sync()
	if err != nil {
		t.Fatal(err)
	}
	content, _, _ := a.Store.ReadFile(head, "tasks/"+id+".md")
	forged := strings.Replace(content, "state: ready", "state: done", 1)
	_, err = a.Store.Mutate(false, func(h string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "innocent-looking update",
			Changes: []gitx.Change{{Path: "tasks/" + id + ".md", Content: forged}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	lint := a.StateLint(true, "maintenance")
	if lint.Code == 0 {
		t.Fatal("forged ready→done not caught")
	}
	if !lintFailuresContain(lint, "illegal transition ready→done") {
		t.Fatalf("failures = %v", lint.Fields["failures"])
	}
	if !lintFailuresContain(lint, "without appending to run-log") {
		t.Fatalf("run-log atomicity miss: %v", lint.Fields["failures"])
	}

	// HALT is now in force.
	if r := a.Claim(id, "agent-a", ""); r.Err != "halted" {
		t.Fatalf("mutation during HALT: %+v", r)
	}
	mustOK(t, a.Resume("lead"))
}

// Anchor tags the head; a later Sync accepts it (ancestry holds).
func TestAnchorRoundTrip(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	createReady(t, a, "anchored")
	r := mustOK(t, a.Anchor())
	tag, _ := r.Fields["tag"].(string)
	if !strings.HasPrefix(tag, "seed-anchor/") {
		t.Fatalf("tag = %q", tag)
	}
	createReady(t, a, "after anchor")
	if _, err := a.Store.Sync(); err != nil {
		t.Fatalf("sync after anchor: %v", err)
	}
}

func TestReportShape(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "reported")
	mustOK(t, a.Claim(id, "agent-a", "1m"))
	later := time.Now().Add(2 * time.Hour)
	a.Now = func() time.Time { return later }
	r := mustOK(t, a.Report(48*time.Hour))
	expired, _ := r.Fields["expired_leases"].([]string)
	if len(expired) != 1 || expired[0] != id {
		t.Fatalf("expired = %v", r.Fields)
	}
}

func lintFailuresContain(r *Result, substr string) bool {
	failures, _ := r.Fields["failures"].([]string)
	for _, f := range failures {
		if strings.Contains(f, substr) {
			return true
		}
	}
	return false
}

// Mirror record round-trip: mapping written one-commit-per-verb with an
// event; MirrorPlan reflects it (idempotence via exported state).
func TestMirrorPlanAndRecord(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "mirrored card")

	r := mustOK(t, a.MirrorPlan())
	actions, _ := json.Marshal(r.Fields["actions"])
	if !strings.Contains(string(actions), `"op":"create"`) || !strings.Contains(string(actions), id) {
		t.Fatalf("plan = %s", actions)
	}
	if rr := a.MirrorRecord(id, 42, "ready", "nobody"); rr.Code == 0 {
		t.Fatal("non-operator record accepted")
	}
	mustOK(t, a.MirrorRecord(id, 42, "ready", "lead"))
	r = mustOK(t, a.MirrorPlan())
	actions, _ = json.Marshal(r.Fields["actions"])
	if string(actions) != "[]" {
		t.Fatalf("recorded card still planned: %s", actions)
	}
	head, _ := a.Store.Sync()
	log, _, _ := a.Store.ReadFile(head, "run-log.jsonl")
	if !strings.Contains(log, `"verb":"mirror-record"`) {
		t.Fatal("mirror-record event missing from run log")
	}
}
