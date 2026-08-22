// Maintenance verbs (open-seed D7/§7.2): deterministic steps the
// seed-maintenance workflow runs under its operator credential — no model
// secrets involved. Each mutation stays one-commit-per-verb.
package task

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/card"
	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
	"github.com/shaunlmason/open-seed-engine/internal/validate"
)

// ReapExpired releases every in_progress card whose lease has expired (§7.1
// reap: a release, not a rejection). Each reap is its own fenced-out act via
// the operator override, writing the handoff stub.
func (sv *Service) ReapExpired(actor string) *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	cards, err := sv.allCards(head)
	if err != nil {
		return errResult(err)
	}
	var reaped, skipped []string
	for _, c := range cards {
		if c.State != "in_progress" || c.Claim == nil {
			continue
		}
		exp, err := time.Parse(time.RFC3339, c.Claim.LeaseExpires)
		if err != nil || !sv.Now().UTC().After(exp) {
			continue
		}
		r := sv.Transition(TransitionArgs{Verb: "transition", ID: c.ID, To: "ready", Actor: actor})
		if r.Code == 0 {
			reaped = append(reaped, c.ID)
		} else {
			skipped = append(skipped, c.ID+":"+r.Err)
		}
	}
	return ok(map[string]any{"verb": "reap", "reaped": reaped, "skipped": skipped})
}

// PlanUnblock removes a plan:<pr> entry from a blocked card — the
// state-shaped plan-unblock auto-path (D1), shim-mediated under the
// maintenance operator credential. The caller (the workflow) has already
// established that the PR is merged or closed; each path removes only its
// own entry, and the transition fires only when the set empties.
func (sv *Service) PlanUnblock(id string, pr int, actor string) *Result {
	if !sv.Cfg.IsOperator(actor) {
		return failure(spec.ExitInvalid, "operator_required", nil)
	}
	entry := "plan:" + strconv.Itoa(pr)
	var newState string
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		c, err := sv.loadCard(head, id)
		if err != nil {
			return nil, err
		}
		if c.State != "blocked" || !slices.Contains(c.BlockedOn, entry) {
			return nil, &stateref.Terminal{Code: spec.ExitInvalid, Name: "invalid_transition"}
		}
		c.BlockedOn = slices.DeleteFunc(slices.Clone(c.BlockedOn), func(e string) bool { return e == entry })
		data := map[string]any{"removed": entry, "auto_path": "plan_unblock"}
		if len(c.BlockedOn) == 0 {
			c.State = "ready"
			data["to"] = "ready"
		}
		newState = c.State
		c.UpdatedAt = sv.now()
		content, err := c.Serialize()
		if err != nil {
			return nil, err
		}
		return &stateref.Mutation{
			Message: "plan-unblock " + id,
			Changes: []gitx.Change{{Path: card.Path(id), Content: content}},
			Events:  []string{sv.event(actor, "plan-unblock", id, data)},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "plan-unblock", "task": id, "state": newState})
}

// Anchor tags the current state-ref head as seed-anchor/<ts> and pushes the
// tag (§7.2 checkpoint anchors — protected tags, never default-branch
// commits). Runs under the maintenance credential; the tag-protection rule
// makes it create-only.
func (sv *Service) Anchor() *Result {
	gitStore, isGit := sv.Store.(*stateref.Store)
	if !isGit {
		// Machine-local stores have no remote to anchor against — the
		// integrity story is local-filesystem trust (declared variance).
		return failure(spec.ExitUnavailable, "anchors_not_applicable", map[string]any{
			"detail": "anchors checkpoint the seed-state ref; the " + sv.Cfg.Coordination.Backend + " backend is machine-local (no remote, no tags)",
		})
	}
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	name := "seed-anchor/" + sv.Now().UTC().Format("20060102T150405Z")
	if _, err := gitStore.Repo.Git("tag", name, head); err != nil {
		return errResult(err)
	}
	if _, err := gitStore.Repo.Git("push", sv.Cfg.Coordination.Remote, "refs/tags/"+name); err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "anchor", "tag": name, "head": head})
}

// Report summarizes coordination health: per-state counts, expired leases
// (reap candidates), stalled reviews, and long-parked plans (R4/§7.1
// reporting concerns — never lease events).
func (sv *Service) Report(stalledAfter time.Duration) *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	cards, err := sv.allCards(head)
	if err != nil {
		return errResult(err)
	}
	states := map[string]int{}
	var expired, stalled, parked []string
	cutoff := sv.Now().UTC().Add(-stalledAfter)
	for _, c := range cards {
		states[c.State]++
		if c.State == "in_progress" && c.Claim != nil {
			if exp, err := time.Parse(time.RFC3339, c.Claim.LeaseExpires); err == nil && sv.Now().UTC().After(exp) {
				expired = append(expired, c.ID)
			}
		}
		updated := c.UpdatedAt
		if updated == "" {
			updated = c.CreatedAt
		}
		if ts, err := time.Parse(time.RFC3339, updated); err == nil && ts.Before(cutoff) {
			switch {
			case c.State == "review":
				stalled = append(stalled, c.ID)
			case c.State == "blocked" && hasPlanEntry(c):
				parked = append(parked, c.ID)
			}
		}
	}
	return ok(map[string]any{"verb": "report", "states": states,
		"expired_leases": expired, "stalled_reviews": stalled, "long_parked_plans": parked})
}

func hasPlanEntry(c *card.Card) bool {
	for _, e := range c.BlockedOn {
		if strings.HasPrefix(e, "plan:") {
			return true
		}
	}
	return false
}

// AncestryWarnings adapts the card set to validate's §6 goal-ancestry
// check (plan os-10c10aae): report-only, computed here because cards
// live behind the store.
func (sv *Service) AncestryWarnings(teams []validate.Team) []string {
	head, err := sv.Store.Sync()
	if err != nil {
		return nil
	}
	cards, err := sv.allCards(head)
	if err != nil {
		return nil
	}
	var ac []validate.AncestryCard
	for _, c := range cards {
		ac = append(ac, validate.AncestryCard{ID: c.ID, Parent: c.Parent, State: c.State, Labels: c.Labels})
	}
	return validate.AncestryWarnings(teams, ac)
}
