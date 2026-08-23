package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Spec-driven output conformance (plan os-61967950): the required verbs'
// declared outputs in verbs.json are asserted against the envelope each
// builtin backend actually produces: the table derives from the spec, so
// a newly declared output fails here until both builtins emit it. The
// hard-coded id tests remain as supplemental coverage.

type verbSpec struct {
	Output json.RawMessage `json:"output"`
}

// declaredKeys returns the output's key set; a non-object declaration
// (e.g. watch's "stream") has no per-key contract to assert.
func (v verbSpec) declaredKeys() map[string]any {
	var m map[string]any
	if json.Unmarshal(v.Output, &m) != nil {
		return nil
	}
	return m
}

func loadDeclaredOutputs(t *testing.T) (required, optional map[string]verbSpec) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "spec", "testdata", "seed", "port-schema", "verbs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Required map[string]verbSpec `json:"required"`
		Optional map[string]verbSpec `json:"optional_capabilities"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Required, doc.Optional
}

// envelopeKeys marshals Result.Fields exactly as cmd/seed emit() does and
// returns the envelope's key set, asserting through the wire shape, not
// Go internals.
func envelopeKeys(t *testing.T, r *Result) map[string]bool {
	t.Helper()
	if r == nil || r.Code != 0 {
		t.Fatalf("verb failed: %+v", r)
	}
	env := map[string]any{"ok": true, "schema_version": "1.0"}
	for k, v := range r.Fields {
		env[k] = v
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("envelope unmarshalable: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for k := range round {
		keys[k] = true
	}
	return keys
}

func TestSpecDeclaredOutputs(t *testing.T) {
	requiredVerbs, optionalVerbs := loadDeclaredOutputs(t)
	services := map[string]func(t *testing.T) *Service{
		"filecards": func(t *testing.T) *Service { return newHarness(t).clone("a") },
		"fastcards": func(t *testing.T) *Service { return fastService(t, "") },
	}
	for backend, mk := range services {
		t.Run(backend, func(t *testing.T) {
			sv := mk(t)
			if r := sv.Init(); r.Code != 0 {
				t.Fatalf("init: %+v", r)
			}

			// One scripted lifecycle produces an envelope per verb.
			results := map[string]*Result{}

			results["create"] = sv.Create(CreateArgs{Title: "A", Body: "work", Actor: "w"})
			idA := results["create"].Fields["task"].(string)
			// A second card, parked on dep:A, makes close's cascade output real.
			rb := sv.Create(CreateArgs{Title: "B", Body: "work", Actor: "w"})
			idB := rb.Fields["task"].(string)
			for _, id := range []string{idA, idB} {
				if r := sv.Transition(TransitionArgs{Verb: "promote", ID: id, Actor: "lead"}); r.Code != 0 {
					t.Fatalf("promote %s: %+v", id, r)
				}
			}
			if r := sv.Transition(TransitionArgs{Verb: "block", ID: idB, Actor: "lead", BlockedOn: "dep:" + idA}); r.Code != 0 {
				t.Fatalf("block: %+v", r)
			}

			results["ready"] = sv.Ready("w", "")
			results["list"] = sv.List("")

			results["claim"] = sv.Claim(idA, "w", "")
			token := results["claim"].Fields["claim_token"].(string)
			results["comment"] = sv.Append("comment", idA, "w", token, "note", "")
			results["attach-evidence"] = sv.Append("commit", idA, "w", token, "", "abc123")
			results["lease-renew"] = sv.LeaseRenew(idA, "w", token, "45m")
			results["release"] = sv.Transition(TransitionArgs{Verb: "release", ID: idA, Actor: "w", Token: token})

			r2 := sv.Claim(idA, "w", "")
			token2 := r2.Fields["claim_token"].(string)
			results["transition"] = sv.Transition(TransitionArgs{Verb: "transition", ID: idA, To: "review", Actor: "w", Token: token2})
			results["close"] = sv.Transition(TransitionArgs{Verb: "close", ID: idA, Actor: "lead", Resolution: "done"})
			results["get"] = sv.Get(idA)

			check := func(verb string, spec verbSpec) {
				declared := spec.declaredKeys()
				if len(declared) == 0 {
					return // nothing declared (event-append)
				}
				res, driven := results[verb]
				if !driven {
					t.Errorf("verb %s declares outputs %v but the conformance script has no driver for it", verb, declared)
					return
				}
				keys := envelopeKeys(t, res)
				for want := range declared {
					if !keys[want] {
						t.Errorf("%s: declared output %q missing from envelope %v", verb, want, keys)
					}
				}
			}
			for verb, spec := range requiredVerbs {
				check(verb, spec)
			}
			// lease-renew is an optional capability both builtins implement.
			if spec, ok := optionalVerbs["lease-renew"]; ok {
				check("lease-renew", spec)
			}
		})
	}
}
