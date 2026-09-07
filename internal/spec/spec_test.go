package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testdataSchema = "testdata/seed/port-schema"

func TestLoadCanonicalSpec(t *testing.T) {
	s, err := Load(testdataSchema)
	if err != nil {
		t.Fatal(err)
	}
	if s.Port.ProtocolVersion != 1 {
		t.Errorf("protocol_version = %d", s.Port.ProtocolVersion)
	}
	if len(s.Port.States) != 7 {
		t.Errorf("states = %v", s.Port.States)
	}
	if len(s.Table.Transitions) != 16 {
		t.Errorf("expected 16 edges (D1 table), got %d", len(s.Table.Transitions))
	}
}

// The engine's named exit-code constants must match the spec registry.
func TestExitCodeConstantsMatchRegistry(t *testing.T) {
	s, err := Load(testdataSchema)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"ok":                  ExitOK,
		"claim_not_granted":   ExitClaimNotGranted,
		"invalid_transition":  ExitInvalid,
		"not_found":           ExitNotFound,
		"backend_unavailable": ExitUnavailable,
		"fenced_out":          ExitFenced,
		"version_mismatch":    ExitVersionMismatch,
	}
	got := map[string]int{}
	for code, ec := range s.Port.ExitCodes {
		n, err := strconv.Atoi(code)
		if err != nil {
			t.Fatalf("non-numeric exit code key %q", code)
		}
		got[ec.Name] = n
	}
	for name, code := range want {
		if got[name] != code {
			t.Errorf("exit code %s: spec=%d engine=%d", name, got[name], code)
		}
	}
}

func TestValidateRejectsBrokenSpecs(t *testing.T) {
	base, err := Load(testdataSchema)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		break_ func(*Spec)
	}{
		{"unknown state", func(s *Spec) { s.Table.Transitions[0].From = "limbo" }},
		{"unknown class", func(s *Spec) { s.Table.Transitions[0].Class = "admin" }},
		{"duplicate edge", func(s *Spec) {
			s.Table.Transitions = append(s.Table.Transitions, s.Table.Transitions[0])
		}},
		{"edge out of terminal", func(s *Spec) {
			s.Table.Transitions = append(s.Table.Transitions, Transition{From: "done", To: "ready", Verb: "x", Class: "operator"})
		}},
		{"unknown effect", func(s *Spec) { s.Table.Transitions[0].Effects = []string{"levitate"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := Load(testdataSchema)
			_ = base
			tc.break_(s)
			if errs := s.Validate(); len(errs) == 0 {
				t.Fatal("broken spec validated clean")
			}
		})
	}
}

func TestCheckVersion(t *testing.T) {
	s, err := Load(testdataSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckVersion(s, "testdata/seed"); err != nil {
		t.Fatalf("matching version: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = CheckVersion(s, dir)
	if _, ok := err.(*VersionMismatch); !ok {
		t.Fatalf("want VersionMismatch, got %v", err)
	}
}

func TestLoadRefusalsAndVersionMismatch(t *testing.T) {
	// Missing directory.
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("empty dir loaded")
	}
	// Unparseable port.json.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "port.json"), []byte("{"), 0o644)
	if _, err := Load(dir); err == nil {
		t.Fatal("bad JSON loaded")
	}
	// An internally inconsistent spec surfaces joined validation errors.
	b, err := os.ReadFile(filepath.Join(testdataSchema, "port.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "port.json"), b, 0o644)
	os.WriteFile(filepath.Join(dir, "transitions.json"),
		[]byte(`{"schema_version":"1.0","effect_vocabulary":{},"transitions":[{"verb":"x","from":"nowhere","to":"nowhither","class":"worker"}],"composite_verbs":{}}`), 0o644)
	_, err = Load(dir)
	if err == nil || !strings.Contains(err.Error(), "invalid spec") {
		t.Fatalf("inconsistent spec loaded: %v", err)
	}

	vm := &VersionMismatch{Repo: 1, Spec: 2}
	if !strings.Contains(vm.Error(), ".seed/version=1") {
		t.Fatalf("VersionMismatch.Error: %q", vm.Error())
	}
}

func TestNeedsTokenDefaults(t *testing.T) {
	no, yes := false, true
	cases := []struct {
		tr   Transition
		want bool
	}{
		{Transition{Class: "worker"}, true},
		{Transition{Class: "operator"}, false},
		{Transition{Class: "worker", RequiresToken: &no}, false},
		{Transition{Class: "operator", RequiresToken: &yes}, true},
	}
	for i, c := range cases {
		if got := c.tr.NeedsToken(); got != c.want {
			t.Fatalf("case %d: NeedsToken=%v want %v", i, got, c.want)
		}
	}
}

func TestValidateStructuralErrors(t *testing.T) {
	f := false
	tr := true
	s := &Spec{}
	s.Port.States = []string{"ready", "done"}
	s.Port.TerminalStates = []string{"done", "vanished"}
	s.Table.EffectVocabulary = map[string]string{"mint_token": "m"}
	s.Table.Transitions = []Transition{
		// claim violating all three claim invariants at once.
		{From: "ready", To: "ready", Verb: "claim", Class: "operator",
			RequiresToken: &tr, Effects: []string{"ghost_effect"},
			OperatorOverrides: []Override{{Name: "o", Effects: []string{"other_ghost"}}}},
		// outgoing edge from a terminal state.
		{From: "done", To: "ready", Verb: "reopen", Class: "operator", RequiresToken: &f},
	}
	s.Table.CompositeVerbs = map[string]CompositeVerb{"finish": {ExpandsTo: "nothing"}}
	errs := s.Validate()
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{
		"terminal state \"vanished\"",
		"outgoing edge from terminal state",
		"claim must not require a token",
		"claim must be worker-class",
		"claim must mint_token",
		"effect \"ghost_effect\" not in vocabulary",
		"override o: effect \"other_ghost\" not in vocabulary",
		"expands to unknown verb",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestRepoProtocolVersionErrors(t *testing.T) {
	if _, err := RepoProtocolVersion(t.TempDir()); err == nil {
		t.Fatal("missing version file tolerated")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RepoProtocolVersion(dir); err == nil {
		t.Fatal("non-numeric version tolerated")
	}
	if err := CheckVersion(&Spec{}, dir); err == nil {
		t.Fatal("CheckVersion swallowed the read error")
	}
}

// TestOlderTablesStillRequireEvidence pins the compatibility half. The
// table ships in each consuming repository's .seed/port-schema/ and moves
// independently of this binary, so a checkout still carrying a protocol-1
// table written before resolution_present existed must not be able to
// accept without evidence: that is the permanently lint-failing card the
// requirement exists to prevent, and moving enforcement into the table is
// what would otherwise have reopened it.
func TestOlderTablesStillRequireEvidence(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("testdata", "seed", "port-schema")
	for _, name := range []string{"port.json", "transitions.json", "verbs.json"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "transitions.json" {
			// Strip the precondition back out: this is the older table.
			var doc map[string]any
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			for _, raw := range doc["transitions"].([]any) {
				tr := raw.(map[string]any)
				if tr["verb"] == "accept" {
					delete(tr, "preconditions")
				}
			}
			if b, err = json.Marshal(doc); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Load(dir)
	if err != nil {
		t.Fatalf("an older table must still load: %v", err)
	}
	var found bool
	for _, tr := range s.Table.Transitions {
		if tr.Verb != "accept" {
			continue
		}
		for _, pc := range tr.Preconditions {
			if pc.Name == "resolution_present" {
				found = true
				if pc.FailError != "resolution_required" || pc.FailDetail == "" {
					t.Fatalf("the upgraded precondition must carry its refusal and its teaching text: %+v", pc)
				}
			}
		}
	}
	if !found {
		t.Fatal("an accept edge from a table predating resolution_present must be upgraded to carry it")
	}
}

// TestAcceptPreconditionsUpgradeAnOlderTable pins the same compatibility
// rule for the plan gate, and pins that the upgrade is per-precondition
// rather than all-or-nothing: a table that predates both gains both, one
// that already declares the evidence rule gains only the plan gate beside
// it, and one that declares both is left exactly as written. A consuming
// repository's .seed/port-schema/ moves independently of this binary, so a
// checkout carrying any of those three shapes must end up enforcing the
// same two rules.
func TestAcceptPreconditionsUpgradeAnOlderTable(t *testing.T) {
	src := filepath.Join("testdata", "seed", "port-schema")
	// load writes the canonical spec into a temp dir with the accept
	// edge's preconditions replaced by keep, and returns what Load made
	// of it.
	load := func(t *testing.T, keep []string) *Spec {
		t.Helper()
		dir := t.TempDir()
		for _, name := range []string{"port.json", "transitions.json", "verbs.json"} {
			b, err := os.ReadFile(filepath.Join(src, name))
			if err != nil {
				t.Fatal(err)
			}
			if name == "transitions.json" {
				var doc map[string]any
				if err := json.Unmarshal(b, &doc); err != nil {
					t.Fatal(err)
				}
				for _, raw := range doc["transitions"].([]any) {
					tr := raw.(map[string]any)
					if tr["verb"] != "accept" {
						continue
					}
					var kept []any
					for _, p := range asSlice(tr["preconditions"]) {
						if slices.Contains(keep, p.(map[string]any)["name"].(string)) {
							kept = append(kept, p)
						}
					}
					if kept == nil {
						delete(tr, "preconditions")
					} else {
						tr["preconditions"] = kept
					}
				}
				if b, err = json.Marshal(doc); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		s, err := Load(dir)
		if err != nil {
			t.Fatalf("the table must still load: %v", err)
		}
		return s
	}
	names := func(s *Spec) []string {
		var out []string
		for _, tr := range s.Table.Transitions {
			if tr.Verb != "accept" {
				continue
			}
			for _, pc := range tr.Preconditions {
				out = append(out, pc.Name)
			}
		}
		slices.Sort(out)
		return out
	}
	want := []string{"plan_or_exemption", "resolution_present"}
	for _, tc := range []struct {
		name string
		keep []string
	}{
		{"a table predating both gains both", nil},
		{"a table declaring only the evidence rule gains the plan gate", []string{"resolution_present"}},
		{"a table declaring only the plan gate gains the evidence rule", []string{"plan_or_exemption"}},
		{"a table declaring both is left as written", want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := names(load(t, tc.keep))
			if !slices.Equal(got, want) {
				t.Fatalf("accept preconditions = %v, want %v", got, want)
			}
		})
	}
	// The upgraded plan gate carries its refusal and its teaching text,
	// naming both ways to satisfy it: the refusal is the only place a
	// caller learns what to do next.
	for _, tr := range load(t, nil).Table.Transitions {
		if tr.Verb != "accept" {
			continue
		}
		for _, pc := range tr.Preconditions {
			if pc.Name != "plan_or_exemption" {
				continue
			}
			if pc.FailError != "plan_required" || pc.FailExit != ExitInvalid {
				t.Fatalf("the plan gate's refusal: %+v", pc)
			}
			for _, want := range []string{"plans/<id>.md", "--no-pr"} {
				if !strings.Contains(pc.FailDetail, want) {
					t.Fatalf("the refusal must name %q: %q", want, pc.FailDetail)
				}
			}
		}
	}
}

// asSlice reads a JSON array that may be absent.
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
