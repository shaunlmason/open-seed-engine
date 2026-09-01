package task

// The done-evidence drills: an accept must evidence the work, and an
// accept that recorded none must be completable. The pair exists
// because the gap between them was a trap: a single missing flag
// produced a done card the D7 lint refuses forever, and `done` is
// terminal, so no transition could ever repair it.

import (
	"strings"
	"testing"

	"github.com/shaunlmason/open-seed-engine/internal/card"
	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

// getCard reads one card off the current head.
func getCard(t *testing.T, sv *Service, id string) *card.Card {
	t.Helper()
	head, err := sv.Store.Sync()
	if err != nil {
		t.Fatal(err)
	}
	c, err := sv.loadCard(head, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// stripEvidence reproduces the pre-refusal card: a done card whose
// accept recorded no evidence. It has to write the field directly,
// because the whole point of the companion change is that no verb can
// produce this state any more, which is also why the repair verb has
// to exist for the cards that already do.
func stripEvidence(t *testing.T, sv *Service, id string) {
	t.Helper()
	if _, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		c, err := sv.loadCard(head, id)
		if err != nil {
			return nil, err
		}
		c.Review.Evidence = ""
		content, err := c.Serialize()
		if err != nil {
			return nil, err
		}
		return &stateref.Mutation{
			Message: "test fixture: strip evidence " + id,
			Changes: []gitx.Change{{Path: card.Path(id), Content: content}},
			Events:  []string{sv.event("lead", "test-fixture", id, nil)},
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// acceptedCard drives one card to done, optionally without evidence by
// writing the review block the way the old accept path did.
func acceptedCard(t *testing.T, sv *Service, resolution string) string {
	t.Helper()
	id := createReady(t, sv, "evidence drill")
	r := mustOK(t, sv.Claim(id, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)
	mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review",
		Actor: "agent-a", Token: tok}))
	mustOK(t, sv.Transition(TransitionArgs{Verb: "close", ID: id, Actor: "lead",
		Resolution: resolution}))
	return id
}

// TestAcceptRequiresEvidence pins the prevention half: accepting is
// claiming the work is finished, and a finished card that names no
// evidence fails the conformance lint with no way back.
func TestAcceptRequiresEvidence(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())

	for _, verb := range []string{"accept", "close"} {
		id := createReady(t, sv, "no evidence")
		r := mustOK(t, sv.Claim(id, "agent-a", ""))
		tok := r.Fields["claim_token"].(string)
		mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review",
			Actor: "agent-a", Token: tok}))

		got := sv.Transition(TransitionArgs{Verb: verb, ID: id, Actor: "lead"})
		if got.Code == 0 || got.Err != "resolution_required" {
			t.Fatalf("%s with no resolution must refuse: code=%d err=%s", verb, got.Code, got.Err)
		}
		detail, _ := got.Fields["detail"].(string)
		if !strings.Contains(detail, "--resolution") || !strings.Contains(detail, "--no-pr") {
			t.Fatalf("the refusal must name both ways to satisfy it: %q", detail)
		}
		// Refused, so the card is untouched and still acceptable.
		if c := getCard(t, sv, id); c.State != "review" || c.Review != nil {
			t.Fatalf("a refused accept must change nothing: state=%s review=%+v", c.State, c.Review)
		}
		mustOK(t, sv.Transition(TransitionArgs{Verb: verb, ID: id, Actor: "lead",
			Resolution: "https://example.invalid/pr/7"}))
		if c := getCard(t, sv, id); c.Review == nil || c.Review.Evidence != "https://example.invalid/pr/7" {
			t.Fatalf("the accepted card carries its evidence: %+v", c.Review)
		}
	}

	// reject is deliberately untouched: it returns the card to ready,
	// where nothing is yet claimed to be finished.
	id := createReady(t, sv, "rejected")
	r := mustOK(t, sv.Claim(id, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)
	mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review",
		Actor: "agent-a", Token: tok}))
	mustOK(t, sv.Transition(TransitionArgs{Verb: "reject", ID: id, Actor: "lead"}))
}

// TestRecordEvidenceCompletesAnAcceptAndNothingElse pins the repair
// half and, just as importantly, its boundaries: it fills an empty
// evidence field and refuses every other use.
func TestRecordEvidenceCompletesAnAcceptAndNothingElse(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())

	// The repair target: a done card whose accept recorded no evidence.
	// Reaching it needs the pre-refusal path, so the review block is
	// written directly, exactly as the old accept wrote it.
	id := acceptedCard(t, sv, "https://example.invalid/pr/1")
	stripEvidence(t, sv, id)
	if c := getCard(t, sv, id); c.State != "done" || c.Review.Evidence != "" {
		t.Fatalf("fixture: want a done card with empty evidence, got %s %+v", c.State, c.Review)
	}
	before := getCard(t, sv, id)

	// A worker may not complete an operator's attestation.
	if got := sv.RecordEvidence(id, "agent-a", "https://example.invalid/pr/2", false); got.Code == 0 ||
		got.Err != "operator_required" {
		t.Fatalf("non-operator must refuse: code=%d err=%s", got.Code, got.Err)
	}
	// Nor may an operator record nothing.
	if got := sv.RecordEvidence(id, "lead", "  ", false); got.Code == 0 || got.Err != "resolution_required" {
		t.Fatalf("an empty resolution must refuse: code=%d err=%s", got.Code, got.Err)
	}

	mustOK(t, sv.RecordEvidence(id, "lead", "https://example.invalid/pr/2", false))
	after := getCard(t, sv, id)
	if after.Review.Evidence != "https://example.invalid/pr/2" {
		t.Fatalf("the evidence must land: %+v", after.Review)
	}
	// It records what the evidence was, never who reviewed or when.
	if after.State != before.State || after.Review.Reviewer != before.Review.Reviewer ||
		after.Review.ReviewedAt != before.Review.ReviewedAt || after.Review.Outcome != "accepted" {
		t.Fatalf("nothing but evidence may change: before=%+v after=%+v", before.Review, after.Review)
	}

	// Idempotence is NOT the contract: an attestation already recorded
	// can never be overwritten, which is what keeps this from being a
	// field-set verb.
	if got := sv.RecordEvidence(id, "lead", "https://example.invalid/pr/3", false); got.Code == 0 ||
		got.Err != "evidence_already_recorded" {
		t.Fatalf("recorded evidence must be immutable: code=%d err=%s", got.Code, got.Err)
	}

	// And it can never fabricate an attestation for a card nobody accepted.
	open := createReady(t, sv, "never accepted")
	if got := sv.RecordEvidence(open, "lead", "https://example.invalid/pr/4", false); got.Code == 0 ||
		got.Err != "no_accepted_review" {
		t.Fatalf("a card with no accepted review must refuse: code=%d err=%s", got.Code, got.Err)
	}

	// The D7 exemption keeps its marker on this path too.
	noPR := acceptedCard(t, sv, "https://example.invalid/pr/5")
	stripEvidence(t, sv, noPR)
	mustOK(t, sv.RecordEvidence(noPR, "lead", "https://example.invalid/artifact", true))
	if e := getCard(t, sv, noPR).Review.Evidence; !strings.HasPrefix(e, "no-pr:") {
		t.Fatalf("the no-PR exemption keeps its prefix: %q", e)
	}
}

// TestRecordEvidenceClearsTheDoneLint is the end-to-end point of the
// pair: the card the lint refuses becomes a card it accepts, without
// any state change at all.
func TestRecordEvidenceClearsTheDoneLint(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	id := acceptedCard(t, sv, "https://example.invalid/pr/1")
	stripEvidence(t, sv, id)

	c := getCard(t, sv, id)
	if fails := sv.lintDone(c); len(fails) == 0 ||
		!strings.Contains(strings.Join(fails, " "), "done without evidence") {
		t.Fatalf("the fixture must reproduce the blocking lint: %v", fails)
	}
	mustOK(t, sv.RecordEvidence(id, "lead", "https://example.invalid/pr/1", false))
	if fails := sv.lintDone(getCard(t, sv, id)); len(fails) != 0 {
		// A missing plan file is the other D7 row and is not this drill's
		// business; only the evidence row must be gone.
		for _, f := range fails {
			if strings.Contains(f, "done without evidence") {
				t.Fatalf("the evidence row must clear: %v", fails)
			}
		}
	}
}
