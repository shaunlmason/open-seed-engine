// Package workflow implements the v2 workflow engine (plan os-52b9aed0,
// design §7.3 + inspirations/04 SYNTHESIS): checked-in step DAGs under
// .seed/workflows/<name>.yaml, validated by thirteen preflight rules and
// executed in topological parallel waves. Workflows are the *intra-run*
// DAG one driver executes; cards remain the inter-agent layer, and any
// task-state mutation a step makes goes through `scripts/seed task <verb>`:
// the executor adds no side channel to a backend.
//
// Run state (checkpoints, artifacts) lives under
// <git-common-dir>/seed-runs/<run-id>/: local, shared across linked
// worktrees, never committed (the inspirations/04 erratum).
package workflow

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

type Input struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Required    bool   `yaml:"required"`
	Description string `yaml:"description"`
}

type Produce struct {
	Name   string `yaml:"name"`
	File   string `yaml:"file"`
	Schema string `yaml:"schema"`
}

// Gate guards the step that carries it and always runs BEFORE the step's
// own action: approval pauses the run until a response file appears;
// review runs the reviewer role and loops through the named remediation
// step on REVISE; checks requires green CI AND zero unresolved review
// threads on the named PR.
type Gate struct {
	Type            string `yaml:"type"` // approval | review | checks
	Message         string `yaml:"message"`
	CaptureResponse bool   `yaml:"capture_response"`
	ReviewerRole    string `yaml:"reviewer_role"`
	Remediation     string `yaml:"remediation"`
	MaxRevisions    int    `yaml:"max_revisions"`
	RequiredCI      bool   `yaml:"required_ci"`
	// pointer so "unresolved_threads: 0" is distinguishable from unset.
	UnresolvedThreads *int   `yaml:"unresolved_threads"`
	Repo              string `yaml:"repo"`
	Ref               string `yaml:"ref"`
	PR                string `yaml:"pr"`
}

type Step struct {
	ID         string `yaml:"id"`
	Role       string `yaml:"role"`
	Harness    string `yaml:"harness"`
	Model      string `yaml:"model"`
	Prompt     string `yaml:"prompt"`
	PromptFile string `yaml:"prompt_file"`
	Run        string `yaml:"run"`
	// tools maps onto the harness contract's SEED_PERMISSION:
	// readonly → read-only, coding → safe-edit (default). yolo is NOT
	// reachable from a workflow file, no tools value maps to it.
	Tools         string         `yaml:"tools"`
	OutputFormat  map[string]any `yaml:"output_format"`
	Consumes      []string       `yaml:"consumes"`
	Produces      []Produce      `yaml:"produces"`
	DependsOn     []string       `yaml:"depends_on"`
	When          string         `yaml:"when"`
	TriggerRule   string         `yaml:"trigger_rule"` // all_success (default) | one_success | all_done
	OnFail        string         `yaml:"on_fail"`      // block (default) | continue
	MaxIterations int            `yaml:"max_iterations"`
	Until         string         `yaml:"until"`
	Steps         []Step         `yaml:"steps"` // loop body
	Gate          *Gate          `yaml:"gate"`
}

type Workflow struct {
	SchemaVersion string  `yaml:"schema_version"`
	Name          string  `yaml:"name"`
	Description   string  `yaml:"description"`
	Inputs        []Input `yaml:"inputs"`
	Defaults      struct {
		Harness string `yaml:"harness"`
		Model   string `yaml:"model"`
	} `yaml:"defaults"`
	Budgets struct {
		MaxWallClockMinutes int `yaml:"max_wall_clock_minutes"`
		MaxStepRetries      int `yaml:"max_step_retries"`
	} `yaml:"budgets"`
	Steps []Step `yaml:"steps"`

	raw []byte // the exact bytes loaded, for the checkpoint hash
}

// Load parses a workflow file strictly: unknown fields fail (validate
// rule 1's structural half; the checked-in workflow.schema.json documents
// the same contract for non-engine consumers).
func Load(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var w Workflow
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	w.raw = raw
	return &w, nil
}

// Registry is the pinned harness/model allowlist (validate rule 9): the
// [workflows] table in .seed/config.toml. A misspelled or unlisted value
// fails preflight, not the first live invocation.
type Registry struct {
	Harnesses []string `toml:"harnesses"`
	Models    []string `toml:"models"`
}

func LoadRegistry(root string) (*Registry, error) {
	var c struct {
		Workflows Registry `toml:"workflows"`
	}
	path := filepath.Join(root, ".seed", "config.toml")
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &c); err != nil {
			return nil, err
		}
	}
	return &c.Workflows, nil
}

// Roles returns the declared role names: the .seed/agents/*.md role
// files (validate rule 8's closure target).
func Roles(root string) (map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(root, ".seed", "agents"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	roles := map[string]bool{}
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".md"); ok {
			roles[n] = true
		}
	}
	return roles, nil
}

// Dir is where workflows live in a template repo.
func Dir(root string) string { return filepath.Join(root, ".seed", "workflows") }

// List returns the checked-in workflow files, sorted.
func List(root string) ([]string, error) {
	entries, err := os.ReadDir(Dir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml") {
			out = append(out, filepath.Join(Dir(root), e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// allSteps yields every step including loop bodies (depth-first).
func allSteps(steps []Step) []*Step {
	var out []*Step
	for i := range steps {
		out = append(out, &steps[i])
		out = append(out, allSteps(steps[i].Steps)...)
	}
	return out
}

// remediationIDs are steps named by a review gate: they exist for the
// gate's revision loop and are excluded from normal wave scheduling.
func remediationIDs(w *Workflow) map[string]bool {
	ids := map[string]bool{}
	for _, s := range allSteps(w.Steps) {
		if s.Gate != nil && s.Gate.Type == "review" && s.Gate.Remediation != "" {
			ids[s.Gate.Remediation] = true
		}
	}
	return ids
}
