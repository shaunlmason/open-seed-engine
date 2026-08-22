// Mirror verbs (D1: component, not backend): plan is read-only over the
// state ref; record writes the card↔issue mapping under the operator
// credential, one commit per verb.
package task

import (
	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/mirror"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

// MirrorPlan computes the export actions for the apply step.
func (sv *Service) MirrorPlan() *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	cards, err := sv.allCards(head)
	if err != nil {
		return errResult(err)
	}
	content, _, err := sv.Store.ReadFile(head, mirror.MapPath)
	if err != nil {
		return errResult(err)
	}
	m, err := mirror.ParseMapping(content)
	if err != nil {
		return errResult(err)
	}
	actions := mirror.Plan(cards, m)
	if actions == nil {
		actions = []mirror.Action{}
	}
	return ok(map[string]any{"verb": "mirror-plan", "actions": actions})
}

// MirrorRecord stores one card's issue mapping and last-exported state.
func (sv *Service) MirrorRecord(id string, issue int, state, actor string) *Result {
	if !sv.Cfg.IsOperator(actor) {
		return failure(spec.ExitInvalid, "operator_required", nil)
	}
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		if _, err := sv.loadCard(head, id); err != nil {
			return nil, err
		}
		content, _, err := sv.Store.ReadFile(head, mirror.MapPath)
		if err != nil {
			return nil, err
		}
		m, err := mirror.ParseMapping(content)
		if err != nil {
			return nil, err
		}
		m.Cards[id] = mirror.Entry{Issue: issue, Exported: state}
		out, err := m.Serialize()
		if err != nil {
			return nil, err
		}
		return &stateref.Mutation{
			Message: "mirror-record " + id,
			Changes: []gitx.Change{{Path: mirror.MapPath, Content: out}},
			Events:  []string{sv.event(actor, "mirror-record", id, map[string]any{"issue": issue, "state": state})},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "mirror-record", "task": id, "issue": issue})
}
