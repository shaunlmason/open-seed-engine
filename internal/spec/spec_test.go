package spec

import (
	"os"
	"path/filepath"
	"strconv"
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
