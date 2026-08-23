// Package port evaluates port verbs against the loaded spec tables. It is a
// pure decision core: given a verb, a card's current coordination state, and a
// credential, it answers whether the operation is legal, which exit code it
// takes, and which shim side effects apply. All legality comes from the spec
// tables (open-seed .seed/port-schema/); this package deliberately contains no
// per-edge branching, so a spec edit changes behavior with no code change.
package port

import (
	"slices"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/spec"
)

type Class string

const (
	Worker   Class = "worker"
	Operator Class = "operator"
)

// Credential is who is asking. Worker credentials carry the claim token where
// one is held; operator credentials are identity-checked by the shim before
// evaluation (which principals hold them is config, not spec).
type Credential struct {
	Class Class
	Actor string
	Token string
}

// Card is the coordination-relevant slice of a task card.
type Card struct {
	State           string
	ClaimToken      string // empty = unclaimed
	LeaseExpired    bool
	RejectedAuthors []string
	BlockedOn       []string
}

// Request is one port operation. To is required when Verb is "transition"
// (or supplied by a composite verb's fixed_to).
type Request struct {
	Verb string
	To   string
}

// Outcome is the decision. Code is a port exit code from port.json. When
// Transitioned is false with Code 0, the verb succeeded without a state
// change (e.g. unblock removed one blocked_on entry of several).
type Outcome struct {
	Code         int
	Err          string
	NewState     string
	Transitioned bool
	Effects      []string
	Override     string // name of the operator override taken, if any
}

func fail(code int, err string) Outcome { return Outcome{Code: code, Err: err} }

// Evaluate decides one request. It never mutates anything: the shim applies
// Effects transactionally (one state-ref commit per verb, §7.2).
func Evaluate(s *spec.Spec, req Request, card Card, cred Credential) Outcome {
	verb, to := req.Verb, req.To
	var extraEffects []string

	if cv, ok := s.Table.CompositeVerbs[verb]; ok {
		if cv.FixedTo != "" {
			to = cv.FixedTo
		}
		extraEffects = cv.AddsEffects
		verb = cv.ExpandsTo
	}

	edge := findEdge(s, verb, to, card.State)
	if edge == nil {
		return fail(spec.ExitInvalid, "invalid_transition")
	}

	effects := edge.Effects
	override := ""
	switch {
	case cred.Class == Class(edge.Class):
		if edge.NeedsToken() && (cred.Token == "" || cred.Token != card.ClaimToken) {
			return fail(spec.ExitFenced, "fenced_out")
		}
	case cred.Class == Operator && len(edge.OperatorOverrides) > 0:
		ov := satisfiedOverride(edge, card)
		if ov == nil {
			return fail(spec.ExitInvalid, "override_guard_not_satisfied")
		}
		effects = ov.Effects
		override = ov.Name
	default:
		return fail(spec.ExitInvalid, string(edge.Class)+"_required")
	}

	if override == "" {
		for _, pc := range edge.Preconditions {
			if err := checkPrecondition(pc, card, cred); err != "" {
				return fail(pc.FailExit, err)
			}
		}
	}

	effects = append(slices.Clone(effects), extraEffects...)

	if edge.Guard == "blocked_on_empty_after_removal" {
		remaining := removeEntries(card.BlockedOn, removalPrefixes(effects, cred))
		if len(remaining) > 0 {
			return Outcome{Code: spec.ExitOK, NewState: card.State, Transitioned: false, Effects: effects}
		}
	}

	return Outcome{Code: spec.ExitOK, NewState: edge.To, Transitioned: true, Effects: effects, Override: override}
}

// findEdge locates the unique edge for (verb, from[, to]). The transition
// verb is disambiguated by target state; named verbs are unique per
// from-state (validated by spec.Validate's duplicate-edge check).
func findEdge(s *spec.Spec, verb, to, from string) *spec.Transition {
	for i := range s.Table.Transitions {
		t := &s.Table.Transitions[i]
		if t.From != from || t.Verb != verb {
			continue
		}
		if verb == "transition" && t.To != to {
			continue
		}
		return t
	}
	return nil
}

func satisfiedOverride(edge *spec.Transition, card Card) *spec.Override {
	for i := range edge.OperatorOverrides {
		ov := &edge.OperatorOverrides[i]
		switch ov.Guard {
		case "lease_expired":
			if card.LeaseExpired {
				return ov
			}
		case "":
			return ov
		}
	}
	return nil
}

func checkPrecondition(pc spec.Precondition, card Card, cred Credential) string {
	switch pc.Name {
	case "unclaimed":
		if card.ClaimToken != "" {
			return pc.FailError
		}
	case "not_rejected_author":
		if slices.Contains(card.RejectedAuthors, cred.Actor) {
			return pc.FailError
		}
	}
	return ""
}

// removalPrefixes maps removal effects to the blocked_on entries they clear.
// remove_blocked_on_manual clears this operator's own manual: entry, every
// unblock path removes only its own entry (D1).
func removalPrefixes(effects []string, cred Credential) []string {
	var out []string
	for _, e := range effects {
		if e == "remove_blocked_on_manual" {
			out = append(out, "manual:"+cred.Actor)
		}
	}
	return out
}

func removeEntries(entries, exact []string) []string {
	var out []string
	for _, e := range entries {
		drop := false
		for _, x := range exact {
			if e == x || strings.TrimSpace(e) == x {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}
