// Package mirror computes the one-way GitHub Issues export (open-seed D1:
// the mirror is a component, not a backend — cards are authoritative and the
// export direction always wins). Plan is a pure function over cards plus the
// state-ref mapping file; idempotence comes from the mapping's recorded
// last-exported state, so no issue state is ever read back.
package mirror

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/shaunlmason/open-seed-engine/internal/card"
)

const MapPath = "mirror/map.json"

// Entry is one card's mapping: the issue number and the card state at last
// export.
type Entry struct {
	Issue    int    `json:"issue"`
	Exported string `json:"exported"`
}

type Mapping struct {
	Cards map[string]Entry `json:"cards"`
}

func ParseMapping(content string) (*Mapping, error) {
	m := &Mapping{Cards: map[string]Entry{}}
	if content == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(content), m); err != nil {
		return nil, fmt.Errorf("%s: %w", MapPath, err)
	}
	if m.Cards == nil {
		m.Cards = map[string]Entry{}
	}
	return m, nil
}

func (m *Mapping) Serialize() (string, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// Action is one desired issue operation for the apply step (gh in the
// maintenance workflow). State is the port state driving it.
type Action struct {
	Op          string   `json:"op"` // create | update | close
	Card        string   `json:"card"`
	Issue       int      `json:"issue,omitempty"`
	Title       string   `json:"title"`
	Labels      []string `json:"labels"`
	Body        string   `json:"body,omitempty"`
	State       string   `json:"state"`
	CloseReason string   `json:"close_reason,omitempty"` // completed | not_planned
}

// stateLabel implements the D1 normative mapping; backlog carries no state
// label.
func stateLabel(state string) []string {
	switch state {
	case "ready", "in_progress", "review", "done", "blocked":
		return []string{"seed:" + state}
	}
	return nil
}

// Body renders the mirrored issue body: the card body fenced as data plus
// the hidden provenance marker (D7).
func Body(c *card.Card) string {
	return fmt.Sprintf("<!-- seed-mirror: %s -->\n\nMirrored from card `%s` — the card is authoritative; label edits here are requests, not state (open-seed D1).\n\n```\n%s\n```\n", c.ID, c.ID, c.Body)
}

// Plan computes the actions for one export pass. Deterministic (sorted by
// card id) and idempotent: a card whose state matches its last export
// produces no action.
func Plan(cards []*card.Card, m *Mapping) []Action {
	var actions []Action
	sorted := append([]*card.Card{}, cards...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, c := range sorted {
		entry, mapped := m.Cards[c.ID]
		if mapped && entry.Exported == c.State {
			continue
		}
		a := Action{Card: c.ID, Title: c.Title, Labels: stateLabel(c.State), State: "open"}
		switch {
		case !mapped:
			a.Op = "create"
			a.Body = Body(c)
		case c.State == "done" || c.State == "cancelled":
			a.Op = "close"
			a.Issue = entry.Issue
		default:
			a.Op = "update"
			a.Issue = entry.Issue
		}
		if c.State == "done" {
			a.State = "closed"
			a.CloseReason = "completed"
		}
		if c.State == "cancelled" {
			a.State = "closed"
			a.CloseReason = "not_planned"
			a.Labels = nil
		}
		actions = append(actions, a)
	}
	return actions
}
