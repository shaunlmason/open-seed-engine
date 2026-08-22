// Package spec loads and validates the port contract from a repository's
// .seed/port-schema/ directory. The spec files are the single authority on
// states, transitions, verb classes, and exit codes (open-seed design §7.5:
// "spec is data"); the engine contains no hand-written transition branching.
package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Exit codes are defined in port.json; these named constants exist only for
// the engine's own control flow and must match the registry (validated in
// tests against the spec file).
const (
	ExitOK              = 0
	ExitClaimNotGranted = 2
	ExitInvalid         = 3
	ExitNotFound        = 4
	ExitUnavailable     = 5
	ExitFenced          = 6
	ExitVersionMismatch = 10
)

type ExitCode struct {
	Name    string `json:"name"`
	Meaning string `json:"meaning"`
}

type Port struct {
	SchemaVersion      string              `json:"schema_version"`
	ProtocolVersion    int                 `json:"protocol_version"`
	States             []string            `json:"states"`
	TerminalStates     []string            `json:"terminal_states"`
	ReinstatableStates []string            `json:"reinstatable_states"`
	ClaimBearingStates []string            `json:"claim_bearing_states"`
	ExitCodes          map[string]ExitCode `json:"exit_codes"`
}

type Precondition struct {
	Name      string `json:"name"`
	FailError string `json:"fail_error"`
	FailExit  int    `json:"fail_exit"`
}

type Override struct {
	Name    string   `json:"name"`
	Guard   string   `json:"guard"`
	Effects []string `json:"effects"`
}

type AutoPath struct {
	Name    string `json:"name"`
	Removes string `json:"removes"`
	Trigger string `json:"trigger"`
}

type Transition struct {
	From              string         `json:"from"`
	To                string         `json:"to"`
	Verb              string         `json:"verb"`
	Class             string         `json:"class"`
	RequiresToken     *bool          `json:"requires_token"`
	Effects           []string       `json:"effects"`
	Preconditions     []Precondition `json:"preconditions"`
	OperatorOverrides []Override     `json:"operator_overrides"`
	Guard             string         `json:"guard"`
	AutoPaths         []AutoPath     `json:"auto_paths"`
}

// NeedsToken reports whether this edge is fenced. Worker edges are fenced by
// default; claim opts out explicitly in the spec (the token-minting bootstrap
// exception, §7.1). Operator edges never present tokens.
func (t *Transition) NeedsToken() bool {
	if t.RequiresToken != nil {
		return *t.RequiresToken
	}
	return t.Class == "worker"
}

type CompositeVerb struct {
	ExpandsTo   string   `json:"expands_to"`
	FixedTo     string   `json:"fixed_to"`
	AddsEffects []string `json:"adds_effects"`
}

type Table struct {
	SchemaVersion    string                   `json:"schema_version"`
	EffectVocabulary map[string]string        `json:"effect_vocabulary"`
	Transitions      []Transition             `json:"transitions"`
	CompositeVerbs   map[string]CompositeVerb `json:"composite_verbs"`
}

type Spec struct {
	Port  Port
	Table Table
}

// Load reads port.json and transitions.json from dir and validates internal
// consistency. Any validation failure is fatal: an inconsistent spec must
// never drive the port.
func Load(dir string) (*Spec, error) {
	var s Spec
	if err := readJSON(filepath.Join(dir, "port.json"), &s.Port); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "transitions.json"), &s.Table); err != nil {
		return nil, err
	}
	if errs := s.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid spec in %s: %s", dir, joinErrs(errs))
	}
	return &s, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func joinErrs(errs []error) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Validate enforces the structural invariants the design states in prose:
// every edge references known states, carries exactly one known class, and no
// (from,to) pair appears twice (each legal transition has exactly one class);
// terminal states have no outgoing edges; claim is the unique token-minting
// edge; all effects come from the declared vocabulary.
func (s *Spec) Validate() []error {
	var errs []error
	stateSet := map[string]bool{}
	for _, st := range s.Port.States {
		stateSet[st] = true
	}
	for _, t := range s.Port.TerminalStates {
		if !stateSet[t] {
			errs = append(errs, fmt.Errorf("terminal state %q not in states", t))
		}
	}
	seenEdge := map[string]string{}
	claimEdges := 0
	for i := range s.Table.Transitions {
		t := &s.Table.Transitions[i]
		id := fmt.Sprintf("%s→%s", t.From, t.To)
		if !stateSet[t.From] {
			errs = append(errs, fmt.Errorf("%s: unknown from-state", id))
		}
		if !stateSet[t.To] {
			errs = append(errs, fmt.Errorf("%s: unknown to-state", id))
		}
		if t.Class != "worker" && t.Class != "operator" {
			errs = append(errs, fmt.Errorf("%s: unknown class %q", id, t.Class))
		}
		if prev, dup := seenEdge[id]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate edge (classes %s and %s)", id, prev, t.Class))
		}
		seenEdge[id] = t.Class
		if slices.Contains(s.Port.TerminalStates, t.From) {
			errs = append(errs, fmt.Errorf("%s: outgoing edge from terminal state", id))
		}
		if t.Verb == "claim" {
			claimEdges++
			if t.NeedsToken() {
				errs = append(errs, fmt.Errorf("%s: claim must not require a token (bootstrap exception)", id))
			}
			if t.Class != "worker" {
				errs = append(errs, fmt.Errorf("%s: claim must be worker-class", id))
			}
			if !slices.Contains(t.Effects, "mint_token") {
				errs = append(errs, fmt.Errorf("%s: claim must mint_token", id))
			}
		}
		for _, e := range t.Effects {
			if _, ok := s.Table.EffectVocabulary[e]; !ok {
				errs = append(errs, fmt.Errorf("%s: effect %q not in vocabulary", id, e))
			}
		}
		for _, ov := range t.OperatorOverrides {
			for _, e := range ov.Effects {
				if _, ok := s.Table.EffectVocabulary[e]; !ok {
					errs = append(errs, fmt.Errorf("%s override %s: effect %q not in vocabulary", id, ov.Name, e))
				}
			}
		}
	}
	if claimEdges != 1 {
		errs = append(errs, fmt.Errorf("expected exactly one claim edge, found %d", claimEdges))
	}
	for name, cv := range s.Table.CompositeVerbs {
		found := false
		for i := range s.Table.Transitions {
			if s.Table.Transitions[i].Verb == cv.ExpandsTo {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("composite verb %q expands to unknown verb %q", name, cv.ExpandsTo))
		}
	}
	return errs
}

// RepoProtocolVersion reads .seed/version from a repo root (the file above
// the port-schema dir). The shim is the enforcement point for exit 10.
func RepoProtocolVersion(seedDir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(seedDir, "version"))
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf(".seed/version: %w", err)
	}
	return v, nil
}

// VersionMismatch describes a protocol-version disagreement (exit 10).
type VersionMismatch struct{ Repo, Spec int }

func (v *VersionMismatch) Error() string {
	return fmt.Sprintf("protocol version mismatch: .seed/version=%d, port.json protocol_version=%d", v.Repo, v.Spec)
}

// CheckVersion verifies the repo's declared protocol version matches the
// loaded spec's. seedDir is the .seed directory containing both `version`
// and `port-schema/`.
func CheckVersion(s *Spec, seedDir string) error {
	repo, err := RepoProtocolVersion(seedDir)
	if err != nil {
		return err
	}
	if repo != s.Port.ProtocolVersion {
		return &VersionMismatch{Repo: repo, Spec: s.Port.ProtocolVersion}
	}
	return nil
}
