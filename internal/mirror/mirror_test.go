package mirror

import (
	"strings"
	"testing"

	"github.com/shaunlmason/open-seed-engine/internal/card"
)

func c(id, state string) *card.Card {
	return &card.Card{ID: id, Title: "T " + id, State: state, Body: "work order"}
}

func TestPlanNewCardCreates(t *testing.T) {
	m, _ := ParseMapping("")
	actions := Plan([]*card.Card{c("os-b", "ready"), c("os-a", "backlog")}, m)
	if len(actions) != 2 || actions[0].Card != "os-a" || actions[1].Card != "os-b" {
		t.Fatalf("actions = %+v", actions)
	}
	if actions[0].Op != "create" || len(actions[0].Labels) != 0 {
		t.Errorf("backlog create: %+v (no state label for backlog)", actions[0])
	}
	if actions[1].Labels[0] != "seed:ready" {
		t.Errorf("ready label: %+v", actions[1])
	}
	if !strings.Contains(actions[0].Body, "<!-- seed-mirror: os-a -->") {
		t.Error("provenance marker missing")
	}
}

func TestPlanIdempotentAndUpdates(t *testing.T) {
	m, _ := ParseMapping(`{"cards":{"os-a":{"issue":7,"exported":"ready"}}}`)
	if actions := Plan([]*card.Card{c("os-a", "ready")}, m); len(actions) != 0 {
		t.Fatalf("unchanged card produced %+v", actions)
	}
	actions := Plan([]*card.Card{c("os-a", "in_progress")}, m)
	if len(actions) != 1 || actions[0].Op != "update" || actions[0].Issue != 7 ||
		actions[0].Labels[0] != "seed:in_progress" {
		t.Fatalf("state change: %+v", actions)
	}
}

func TestPlanCloseSemantics(t *testing.T) {
	m, _ := ParseMapping(`{"cards":{"os-a":{"issue":7,"exported":"review"},"os-b":{"issue":8,"exported":"ready"}}}`)
	actions := Plan([]*card.Card{c("os-a", "done"), c("os-b", "cancelled")}, m)
	if len(actions) != 2 {
		t.Fatalf("actions = %+v", actions)
	}
	if actions[0].Op != "close" || actions[0].CloseReason != "completed" || actions[0].Labels[0] != "seed:done" {
		t.Errorf("done close: %+v", actions[0])
	}
	if actions[1].Op != "close" || actions[1].CloseReason != "not_planned" || actions[1].Labels != nil {
		t.Errorf("cancelled close: %+v", actions[1])
	}
}

func TestMappingRoundTrip(t *testing.T) {
	m, _ := ParseMapping("")
	m.Cards["os-a"] = Entry{Issue: 3, Exported: "ready"}
	s, err := m.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseMapping(s)
	if err != nil || back.Cards["os-a"].Issue != 3 {
		t.Fatalf("round trip: %v %+v", err, back)
	}
}

func TestParseMappingEdges(t *testing.T) {
	m, err := ParseMapping("")
	if err != nil || m == nil || m.Cards == nil {
		t.Fatalf("empty mapping: %+v %v", m, err)
	}
	if _, err := ParseMapping("{broken"); err == nil {
		t.Fatal("broken mapping parsed")
	}
	m, err = ParseMapping(`{"cards": null}`)
	if err != nil || m.Cards == nil {
		t.Fatalf("null cards not defaulted: %+v %v", m, err)
	}
}
