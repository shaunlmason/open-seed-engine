package task

import (
	"os"
	"path/filepath"
	"testing"
)

// §6 routing activation (plan os-10c10aae): explicit squad → lowest-
// priority backlog match → core fallback; ready --squad filters on the
// resolved squad, so no card is invisible.
func TestSquadRoutingResolution(t *testing.T) {
	h := newHarness(t)
	sv := h.clone("a")
	mustOK(t, sv.Init())
	teams := filepath.Join(sv.Root, ".seed", "teams")
	if err := os.MkdirAll(teams, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(teams, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("core.yaml", "name: core\nlead: alice\nscope: [\"**\"]\npriority: 1\ntier: L1\nreview: codeowners\n")
	write("web.yaml", "name: web\nlead: bob\nscope: [\"web/**\"]\nbacklog: {labels: [frontend]}\npriority: 2\ntier: L1\nreview: codeowners\n")

	explicit := mustOK(t, sv.Create(CreateArgs{Title: "E", Actor: "a", Squad: "web"})).Fields["task"].(string)
	labeled := mustOK(t, sv.Create(CreateArgs{Title: "L", Actor: "a", Labels: []string{"frontend"}})).Fields["task"].(string)
	fallback := mustOK(t, sv.Create(CreateArgs{Title: "F", Actor: "a"})).Fields["task"].(string)
	for _, id := range []string{explicit, labeled, fallback} {
		mustOK(t, sv.Transition(TransitionArgs{Verb: "promote", ID: id, Actor: "lead"}))
	}

	if g := mustOK(t, sv.Get(labeled)); g.Fields["squad"] != "web" {
		t.Fatalf("labeled card routed to %v, want web", g.Fields["squad"])
	}
	if g := mustOK(t, sv.Get(fallback)); g.Fields["squad"] != "core" {
		t.Fatalf("unmatched card routed to %v, want core fallback", g.Fields["squad"])
	}

	r := mustOK(t, sv.Ready("w", "web"))
	tasks := r.Fields["tasks"].([]map[string]any)
	ids := map[string]bool{}
	for _, e := range tasks {
		ids[e["task"].(string)] = true
	}
	if !ids[explicit] || !ids[labeled] || ids[fallback] {
		t.Fatalf("ready --squad web = %v (want explicit+labeled, not fallback)", ids)
	}
	if got := len(mustOK(t, sv.Ready("w", "core")).Fields["tasks"].([]map[string]any)); got != 1 {
		t.Fatalf("ready --squad core = %d cards, want 1", got)
	}
}
