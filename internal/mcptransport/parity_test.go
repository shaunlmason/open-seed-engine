package mcptransport

// The transport parity drill. `seed mcp serve` promises one tool per port
// verb, and the tool set was a hand-maintained list, so a verb added to the
// CLI could silently never reach MCP. That is not hypothetical: it is how
// record-evidence shipped reachable from the CLI and not from MCP.
//
// So parity is asserted against the TABLE rather than against another list:
// every named transition verb, every composite, and every declared
// outside-table verb must have a tool. A verb that exists only in a hand
// list on one side can no longer hide.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaunlmason/open-seed-engine/internal/spec"
)

// toolFor is the naming rule the transport follows: task_<verb>, with the
// verb's dashes as underscores.
func toolFor(verb string) string {
	return "task_" + strings.ReplaceAll(verb, "-", "_")
}

func TestEveryPortVerbHasATool(t *testing.T) {
	s, err := spec.Load(filepath.Join("..", "spec", "testdata", "seed", "port-schema"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tl := range append(Tools(), operatorTools()...) {
		have[tl.Name] = true
	}

	want := map[string]string{}
	for _, tr := range s.Table.Transitions {
		if tr.Verb == "transition" {
			// The generic verb is one tool taking --to, not one per state.
			want["task_transition"] = "transition"
			continue
		}
		want[toolFor(tr.Verb)] = tr.Verb
	}
	for cv := range s.Table.CompositeVerbs {
		want[toolFor(cv)] = cv
	}
	for _, v := range s.Table.VerbsOutsideTable() {
		want[toolFor(v)] = v
	}
	// accept is reachable as close, the composite the table declares for
	// it; the transport exposes the composite rather than both.
	delete(want, "task_accept")

	for tool, verb := range want {
		if !have[tool] {
			t.Errorf("port verb %q has no MCP tool %q: the transport promises one tool per port verb", verb, tool)
		}
	}
	if len(s.Table.VerbsOutsideTable()) == 0 {
		t.Fatal("the table declares no outside-table verbs, so this drill would assert nothing")
	}
}

// TestAcceptingToolsRequireEvidence pins the contract the schema advertises
// against the one the port enforces. A tool that advertised resolution as
// optional would hand a generated client a refusal its own schema said
// could not happen.
func TestAcceptingToolsRequireEvidence(t *testing.T) {
	for _, tl := range append(Tools(), operatorTools()...) {
		if tl.Name != "task_close" && tl.Name != "task_record_evidence" {
			continue
		}
		req, _ := tl.InputSchema["required"].([]string)
		var found bool
		for _, r := range req {
			if r == "resolution" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s must advertise resolution as required: the accept edge refuses without it (%v)", tl.Name, req)
		}
	}
}
