package port

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shaunlmason/open-seed-engine/internal/spec"
)

func loadSpec(t *testing.T) *spec.Spec {
	t.Helper()
	s, err := spec.Load(filepath.Join("..", "spec", "testdata", "seed", "port-schema"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Conformance: every cell of transition table × verb class × token validity
// (build plan Phase 1). Expectations are computed by an independent
// mini-interpreter over the same spec tables, so the engine and the test can
// only agree by both following the table.
func TestConformanceExhaustive(t *testing.T) {
	s := loadSpec(t)
	verbs := allVerbs(s)
	creds := []struct {
		name string
		cred Credential
	}{
		{"worker_good_token", Credential{Class: Worker, Actor: "w1", Token: "tok"}},
		{"worker_stale_token", Credential{Class: Worker, Actor: "w1", Token: "stale"}},
		{"worker_no_token", Credential{Class: Worker, Actor: "w1"}},
		{"operator", Credential{Class: Operator, Actor: "op1"}},
	}
	for _, state := range s.Port.States {
		for _, v := range verbs {
			for _, c := range creds {
				for _, leaseExpired := range []bool{false, true} {
					card := Card{State: state, LeaseExpired: leaseExpired}
					// Cards in claim-bearing states hold a claim with token "tok".
					if slices.Contains(s.Port.ClaimBearingStates, state) {
						card.ClaimToken = "tok"
					}
					got := Evaluate(s, v.req, card, c.cred)
					want := expect(s, v.req, card, c.cred)
					if got.Code != want.code || got.Transitioned != want.transitioned ||
						(got.Transitioned && got.NewState != want.newState) {
						t.Errorf("state=%s verb=%s to=%s cred=%s lease_expired=%v:\n got (code=%d err=%q new=%s trans=%v)\nwant (code=%d new=%s trans=%v)",
							state, v.req.Verb, v.req.To, c.name, leaseExpired,
							got.Code, got.Err, got.NewState, got.Transitioned,
							want.code, want.newState, want.transitioned)
					}
				}
			}
		}
	}
}

type verbCase struct{ req Request }

// allVerbs enumerates every verb surface the table defines: the generic
// transition verb toward every state, every named verb, and every composite.
func allVerbs(s *spec.Spec) []verbCase {
	var out []verbCase
	named := map[string]bool{}
	for _, tr := range s.Table.Transitions {
		if tr.Verb == "transition" {
			continue
		}
		named[tr.Verb] = true
	}
	// Every request carries a resolution, and every request is repeated
	// without one: the resolution_present precondition on the accept edge
	// has two sides, and a sweep that only ever passed evidence would
	// never reach the refusing one.
	for v := range named {
		out = append(out, verbCase{Request{Verb: v, Resolution: "https://example.invalid/pr/1"}})
		out = append(out, verbCase{Request{Verb: v}})
	}
	for cv := range s.Table.CompositeVerbs {
		out = append(out, verbCase{Request{Verb: cv, Resolution: "https://example.invalid/pr/1"}})
		out = append(out, verbCase{Request{Verb: cv}})
	}
	for _, to := range s.Port.States {
		out = append(out, verbCase{Request{Verb: "transition", To: to, Resolution: "https://example.invalid/pr/1"}})
		out = append(out, verbCase{Request{Verb: "transition", To: to}})
	}
	return out
}

type expectation struct {
	code         int
	newState     string
	transitioned bool
}

// expect is the independent interpreter: a second, minimal reading of the
// spec semantics (§7.1 prose, encoded once more).
func expect(s *spec.Spec, req Request, card Card, cred Credential) expectation {
	verb, to := req.Verb, req.To
	if cv, ok := s.Table.CompositeVerbs[verb]; ok {
		if cv.FixedTo != "" {
			to = cv.FixedTo
		}
		verb = cv.ExpandsTo
	}
	var edge *spec.Transition
	for i := range s.Table.Transitions {
		tr := &s.Table.Transitions[i]
		if tr.From == card.State && tr.Verb == verb && (verb != "transition" || tr.To == to) {
			edge = tr
			break
		}
	}
	if edge == nil {
		return expectation{code: spec.ExitInvalid}
	}
	if cred.Class != Class(edge.Class) {
		if cred.Class == Operator {
			for _, ov := range edge.OperatorOverrides {
				if ov.Guard == "lease_expired" && card.LeaseExpired || ov.Guard == "" {
					return expectation{code: spec.ExitOK, newState: edge.To, transitioned: true}
				}
			}
		}
		return expectation{code: spec.ExitInvalid}
	}
	if edge.NeedsToken() && (cred.Token == "" || cred.Token != card.ClaimToken) {
		return expectation{code: spec.ExitFenced}
	}
	for _, pc := range edge.Preconditions {
		switch pc.Name {
		case "resolution_present":
			if strings.TrimSpace(req.Resolution) == "" {
				return expectation{code: pc.FailExit}
			}
		case "unclaimed":
			if card.ClaimToken != "" {
				return expectation{code: pc.FailExit}
			}
		case "not_rejected_author":
			if slices.Contains(card.RejectedAuthors, cred.Actor) {
				return expectation{code: pc.FailExit}
			}
		}
	}
	if edge.Guard == "blocked_on_empty_after_removal" && len(card.BlockedOn) > 0 {
		remaining := 0
		for _, e := range card.BlockedOn {
			if e != "manual:"+cred.Actor {
				remaining++
			}
		}
		if remaining > 0 {
			return expectation{code: spec.ExitOK, newState: card.State, transitioned: false}
		}
	}
	return expectation{code: spec.ExitOK, newState: edge.To, transitioned: true}
}

// The design-doc invariants, asserted directly (not via the interpreter).
func TestDesignInvariants(t *testing.T) {
	s := loadSpec(t)
	tok := Credential{Class: Worker, Actor: "w1", Token: "tok"}
	op := Credential{Class: Operator, Actor: "lead"}

	t.Run("claim mints token and needs none", func(t *testing.T) {
		got := Evaluate(s, Request{Verb: "claim"}, Card{State: "ready"}, Credential{Class: Worker, Actor: "w1"})
		if got.Code != 0 || !slices.Contains(got.Effects, "mint_token") {
			t.Fatalf("claim from ready = %+v", got)
		}
	})
	t.Run("claim contention exits 2", func(t *testing.T) {
		got := Evaluate(s, Request{Verb: "claim"}, Card{State: "ready", ClaimToken: "other"}, Credential{Class: Worker, Actor: "w1"})
		if got.Code != spec.ExitClaimNotGranted || got.Err != "claim_contention" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("rejected author locked out with exit 2", func(t *testing.T) {
		got := Evaluate(s, Request{Verb: "claim"}, Card{State: "ready", RejectedAuthors: []string{"w1"}}, Credential{Class: Worker, Actor: "w1"})
		if got.Code != spec.ExitClaimNotGranted || got.Err != "rejected_author" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("stale and missing tokens are fenced with exit 6", func(t *testing.T) {
		card := Card{State: "in_progress", ClaimToken: "tok"}
		for _, cred := range []Credential{{Class: Worker, Actor: "w1", Token: "stale"}, {Class: Worker, Actor: "w1"}} {
			if got := Evaluate(s, Request{Verb: "transition", To: "review"}, card, cred); got.Code != spec.ExitFenced {
				t.Fatalf("cred %+v: got %+v", cred, got)
			}
		}
	})
	t.Run("every exit from in_progress ends the claim", func(t *testing.T) {
		for _, tr := range s.Table.Transitions {
			if tr.From != "in_progress" {
				continue
			}
			if !slices.Contains(tr.Effects, "end_claim") {
				t.Errorf("in_progress→%s lacks end_claim", tr.To)
			}
		}
	})
	t.Run("operator cancel from in_progress clears claim and writes handoff", func(t *testing.T) {
		got := Evaluate(s, Request{Verb: "cancel"}, Card{State: "in_progress", ClaimToken: "tok"}, op)
		if got.Code != 0 || !slices.Contains(got.Effects, "end_claim") || !slices.Contains(got.Effects, "write_handoff") || !slices.Contains(got.Effects, "cascade") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("reap needs operator credential and expired lease", func(t *testing.T) {
		card := Card{State: "in_progress", ClaimToken: "tok"}
		if got := Evaluate(s, Request{Verb: "transition", To: "ready"}, card, op); got.Code != spec.ExitInvalid {
			t.Fatalf("live lease reap allowed: %+v", got)
		}
		card.LeaseExpired = true
		got := Evaluate(s, Request{Verb: "transition", To: "ready"}, card, op)
		if got.Code != 0 || got.Override != "reap" || !slices.Contains(got.Effects, "write_handoff") {
			t.Fatalf("expired lease reap: %+v", got)
		}
		if slices.Contains(got.Effects, "append_rejected_author") {
			t.Fatal("reap must not reject")
		}
	})
	t.Run("close is accept plus cascade, only from review", func(t *testing.T) {
		closing := Request{Verb: "close", Resolution: "https://example.invalid/pr/1"}
		got := Evaluate(s, closing, Card{State: "review"}, op)
		if got.Code != 0 || got.NewState != "done" || !slices.Contains(got.Effects, "cascade") || !slices.Contains(got.Effects, "record_review") {
			t.Fatalf("close from review: %+v", got)
		}
		for _, st := range []string{"backlog", "ready", "in_progress", "blocked", "done", "cancelled"} {
			if got := Evaluate(s, closing, Card{State: st}, op); got.Code != spec.ExitInvalid {
				t.Errorf("close from %s: %+v", st, got)
			}
		}
	})
	t.Run("close and accept refuse without evidence, and say why", func(t *testing.T) {
		// The requirement is the accept edge's precondition, so close
		// inherits it by expansion rather than by a second rule, and the
		// refusal carries the table's own explanation.
		for _, verb := range []string{"accept", "close"} {
			got := Evaluate(s, Request{Verb: verb}, Card{State: "review"}, op)
			if got.Code != spec.ExitInvalid || got.Err != "resolution_required" {
				t.Fatalf("%s with no resolution: %+v", verb, got)
			}
			if !strings.Contains(got.Detail, "--resolution") || !strings.Contains(got.Detail, "--no-pr") {
				t.Fatalf("%s must teach both ways to satisfy it: %q", verb, got.Detail)
			}
		}
	})
	t.Run("reject appends rejected author", func(t *testing.T) {
		got := Evaluate(s, Request{Verb: "reject"}, Card{State: "review"}, op)
		if got.Code != 0 || got.NewState != "ready" || !slices.Contains(got.Effects, "append_rejected_author") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("operator verbs refuse worker credentials", func(t *testing.T) {
		for verb, state := range map[string]string{"accept": "review", "cancel": "ready", "promote": "backlog", "reinstate": "cancelled", "unblock": "blocked"} {
			if got := Evaluate(s, Request{Verb: verb}, Card{State: state}, tok); got.Code != spec.ExitInvalid {
				t.Errorf("%s as worker: %+v", verb, got)
			}
		}
	})
	t.Run("done is terminal for every verb", func(t *testing.T) {
		for _, v := range allVerbs(s) {
			for _, cred := range []Credential{tok, op} {
				if got := Evaluate(s, v.req, Card{State: "done"}, cred); got.Code != spec.ExitInvalid {
					t.Errorf("verb %s/%s on done: %+v", v.req.Verb, v.req.To, got)
				}
			}
		}
	})
	t.Run("unblock removes only its own entry and fires only when empty", func(t *testing.T) {
		multi := Card{State: "blocked", BlockedOn: []string{"manual:lead", "dep:os-1234"}}
		got := Evaluate(s, Request{Verb: "unblock"}, multi, op)
		if got.Code != 0 || got.Transitioned {
			t.Fatalf("multi-entry unblock transitioned: %+v", got)
		}
		single := Card{State: "blocked", BlockedOn: []string{"manual:lead"}}
		got = Evaluate(s, Request{Verb: "unblock"}, single, op)
		if got.Code != 0 || !got.Transitioned || got.NewState != "ready" {
			t.Fatalf("single-entry unblock: %+v", got)
		}
	})
	t.Run("release composite is the fenced worker release", func(t *testing.T) {
		card := Card{State: "in_progress", ClaimToken: "tok"}
		got := Evaluate(s, Request{Verb: "release"}, card, tok)
		if got.Code != 0 || got.NewState != "ready" || !slices.Contains(got.Effects, "write_handoff") {
			t.Fatalf("release: %+v", got)
		}
		if got := Evaluate(s, Request{Verb: "release"}, card, Credential{Class: Worker, Actor: "w1", Token: "stale"}); got.Code != spec.ExitFenced {
			t.Fatalf("stale release: %+v", got)
		}
	})
}

// The spec, not the code, is the authority: mutating the loaded table must
// change engine behavior with no engine code change (§7.5 / Phase 1 done-when).
func TestSpecIsAuthority(t *testing.T) {
	s := loadSpec(t)
	op := Credential{Class: Operator, Actor: "lead"}

	if got := Evaluate(s, Request{Verb: "reopen"}, Card{State: "done"}, op); got.Code != spec.ExitInvalid {
		t.Fatalf("reopen before spec edit: %+v", got)
	}
	s.Table.Transitions = append(s.Table.Transitions, spec.Transition{
		From: "done", To: "backlog", Verb: "reopen", Class: "operator", Effects: []string{},
	})
	if got := Evaluate(s, Request{Verb: "reopen"}, Card{State: "done"}, op); got.Code != 0 || got.NewState != "backlog" {
		t.Fatalf("reopen after spec edit: %+v", got)
	}

	s2 := loadSpec(t)
	kept := s2.Table.Transitions[:0]
	for _, tr := range s2.Table.Transitions {
		if !(tr.From == "ready" && tr.To == "blocked") {
			kept = append(kept, tr)
		}
	}
	s2.Table.Transitions = kept
	if got := Evaluate(s2, Request{Verb: "block"}, Card{State: "ready"}, op); got.Code != spec.ExitInvalid {
		t.Fatalf("block after edge removal: %+v", got)
	}
}
