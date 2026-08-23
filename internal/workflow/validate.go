package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding is one validate-rule violation. Rule numbers follow the decided
// list in inspirations/04 (SYNTHESIS, "CI validate rules").
type Finding struct {
	Rule int    `json:"rule"`
	Msg  string `json:"msg"`
}

func (f Finding) String() string { return fmt.Sprintf("rule %d: %s", f.Rule, f.Msg) }

var (
	kebabRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	tokenRe = regexp.MustCompile(`\{\{\s*([a-z_]+)\.([A-Za-z0-9_-]+)(\.[A-Za-z0-9_.-]+)?\s*\}\}`)
)

// Validate runs the thirteen preflight rules against one workflow file.
// withHarnesses additionally checks harness adapters on disk (rule 13).
func Validate(root, path string, withHarnesses bool) []Finding {
	var out []Finding
	add := func(rule int, msg string, a ...any) { out = append(out, Finding{rule, fmt.Sprintf(msg, a...)}) }

	w, err := Load(path)
	if err != nil {
		// Strict decode failure = rule 1 (unknown fields / malformed).
		add(1, "%v", err)
		return out
	}

	// Rule 2: schema_version match.
	if w.SchemaVersion != "1" {
		add(2, "schema_version %q is not \"1\"", w.SchemaVersion)
	}
	// Rule 1 (structural remainder): name must match filename.
	base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".yaml"), ".yml")
	if w.Name != base {
		add(1, "name %q must match filename %q", w.Name, base)
	}
	if len(w.Steps) == 0 {
		add(1, "workflow has no steps")
	}

	steps := allSteps(w.Steps)
	byID := map[string]*Step{}
	// Rule 3: unique kebab-case ids.
	for _, s := range steps {
		if s.ID == "" {
			add(3, "a step is missing an id")
			continue
		}
		if !kebabRe.MatchString(s.ID) {
			add(3, "step id %q is not kebab-case", s.ID)
		}
		if byID[s.ID] != nil {
			add(3, "duplicate step id %q", s.ID)
		}
		byID[s.ID] = s
	}

	// Rule 4: depends_on existence + DAG acyclicity (top-level graph).
	for _, s := range steps {
		for _, d := range s.DependsOn {
			if byID[d] == nil {
				add(4, "step %q depends_on unknown step %q", s.ID, d)
			}
		}
	}
	if cycle := findCycle(w.Steps); cycle != "" {
		add(4, "dependency cycle through %q", cycle)
	}

	remediation := remediationIDs(w)
	inputs := map[string]bool{}
	for _, in := range w.Inputs {
		inputs[in.Name] = true
	}
	reg, regErr := LoadRegistry(root)
	if regErr != nil {
		add(9, "cannot read the [workflows] registry: %v", regErr)
	}
	roles, rolesErr := Roles(root)
	if rolesErr != nil {
		add(8, "cannot read role definitions: %v", rolesErr)
	}

	// producers: artifact name -> producing step id.
	producers := map[string]string{}
	for _, s := range steps {
		for _, p := range s.Produces {
			if p.Name == "" || p.File == "" {
				add(7, "step %q declares a produce without name+file", s.ID)
				continue
			}
			producers[p.Name] = s.ID
		}
	}

	for _, s := range steps {
		actions := 0
		for _, v := range []string{s.Prompt, s.PromptFile, s.Run} {
			if v != "" {
				actions++
			}
		}
		isLoop := len(s.Steps) > 0
		// Rule 5: XOR of prompt|prompt_file|run. Gate-only steps and loop
		// groups carry no action of their own.
		switch {
		case isLoop && actions != 0:
			add(5, "loop step %q may not also carry prompt/prompt_file/run", s.ID)
		case !isLoop && s.Gate == nil && actions != 1:
			add(5, "step %q needs exactly one of prompt|prompt_file|run (has %d)", s.ID, actions)
		case !isLoop && s.Gate != nil && actions > 1:
			add(5, "gate step %q may carry at most one action (has %d)", s.ID, actions)
		}

		// Rule 6: referenced files exist (relative to the workflows dir).
		if s.PromptFile != "" {
			if _, err := os.Stat(filepath.Join(Dir(root), s.PromptFile)); err != nil {
				add(6, "step %q prompt_file %q: %v", s.ID, s.PromptFile, err)
			}
		}
		for _, p := range s.Produces {
			if p.Schema != "" {
				if _, err := os.Stat(filepath.Join(Dir(root), p.Schema)); err != nil {
					add(6, "step %q produce %q schema %q: %v", s.ID, p.Name, p.Schema, err)
				}
			}
		}

		// Rule 7: artifact closure, every consumes is produced by a step
		// reachable through depends_on.
		reach := reachable(s, byID)
		for _, c := range s.Consumes {
			prod, ok := producers[c]
			if !ok {
				add(7, "step %q consumes %q, which nothing produces", s.ID, c)
			} else if !reach[prod] {
				add(7, "step %q consumes %q from step %q, which is not reachable through depends_on", s.ID, c, prod)
			}
		}

		// Rule 8: role closure.
		if s.Role != "" && !roles[s.Role] {
			add(8, "step %q role %q has no .seed/agents/%s.md", s.ID, s.Role, s.Role)
		}
		if s.Gate != nil && s.Gate.Type == "review" && s.Gate.ReviewerRole != "" && !roles[s.Gate.ReviewerRole] {
			add(8, "step %q reviewer_role %q has no role file", s.ID, s.Gate.ReviewerRole)
		}

		// Rule 9: harness AND model against the pinned registry.
		if reg != nil {
			if h := firstNonEmpty(s.Harness, w.Defaults.Harness); h != "" && !contains(reg.Harnesses, h) {
				add(9, "step %q harness %q is not in the [workflows] registry", s.ID, h)
			}
			if m := firstNonEmpty(s.Model, w.Defaults.Model); m != "" && !contains(reg.Models, m) {
				add(9, "step %q model %q is not in the [workflows] registry", s.ID, m)
			}
		}

		// Enumerations (rule 1 structural).
		if s.Tools != "" && s.Tools != "readonly" && s.Tools != "coding" {
			add(1, "step %q tools %q is not readonly|coding (yolo is not reachable from a workflow file)", s.ID, s.Tools)
		}
		if s.TriggerRule != "" && s.TriggerRule != "all_success" && s.TriggerRule != "one_success" && s.TriggerRule != "all_done" {
			add(1, "step %q trigger_rule %q is not all_success|one_success|all_done", s.ID, s.TriggerRule)
		}
		if s.OnFail != "" && s.OnFail != "block" && s.OnFail != "continue" {
			add(1, "step %q on_fail %q is not block|continue", s.ID, s.OnFail)
		}
		if s.Gate != nil {
			switch s.Gate.Type {
			case "approval", "checks":
			case "review":
				if s.Gate.Remediation == "" {
					add(1, "review gate on %q must name a remediation step (re-reviewing an unchanged implementation is the loop's failure mode)", s.ID)
				} else if byID[s.Gate.Remediation] == nil {
					add(1, "review gate on %q names unknown remediation step %q", s.ID, s.Gate.Remediation)
				}
			default:
				add(1, "step %q gate type %q is not approval|review|checks", s.ID, s.Gate.Type)
			}
		}
		if remediation[s.ID] && len(s.DependsOn) > 0 {
			add(1, "remediation step %q is gate-driven and may not declare depends_on", s.ID)
		}

		// Rule 11: loop requirements.
		if isLoop {
			if s.Until == "" {
				add(11, "loop step %q requires until", s.ID)
			}
			if s.MaxIterations <= 0 {
				add(11, "loop step %q requires max_iterations > 0", s.ID)
			}
		} else if s.Until != "" {
			add(11, "step %q sets until without a loop body", s.ID)
		}

		// Rule 12: template-token lint.
		for _, text := range []string{s.Prompt, s.Run, s.When, s.Until} {
			for _, m := range tokenRe.FindAllStringSubmatch(text, -1) {
				switch m[1] {
				case "inputs":
					if !inputs[m[2]] {
						add(12, "step %q references undeclared {{inputs.%s}}", s.ID, m[2])
					}
				case "output":
					prod, ok := producers[m[2]]
					if !ok {
						add(12, "step %q references {{output.%s...}}, which nothing produces", s.ID, m[2])
					} else if !reach[prod] && prod != s.ID {
						add(12, "step %q references {{output.%s...}} from unreachable step %q", s.ID, m[2], prod)
					}
				case "steps":
					// when-expressions reference steps.<id>.outcome.
					if byID[m[2]] == nil {
						add(12, "step %q references unknown step in {{steps.%s...}}", s.ID, m[2])
					}
				default:
					add(12, "step %q has unresolvable token namespace {{%s...}}", s.ID, m[1])
				}
			}
		}

		// Rule 13 (optional): harness adapter on PATH-equivalent.
		if withHarnesses {
			if h := firstNonEmpty(s.Harness, w.Defaults.Harness); h != "" && (s.Prompt != "" || s.PromptFile != "") {
				if _, err := exec.LookPath(filepath.Join(root, "scripts", "harness", h)); err != nil {
					if _, serr := os.Stat(filepath.Join(root, "scripts", "harness", h)); serr != nil {
						add(13, "step %q harness adapter scripts/harness/%s not present", s.ID, h)
					}
				}
			}
		}
	}

	// Rule 10: budget sanity.
	if w.Budgets.MaxWallClockMinutes < 0 || w.Budgets.MaxStepRetries < 0 {
		add(10, "budgets must be non-negative")
	}

	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// reachable returns every step id reachable from s through depends_on
// (transitively), excluding s itself unless it is in a cycle.
func reachable(s *Step, byID map[string]*Step) map[string]bool {
	seen := map[string]bool{}
	var walk func(ids []string)
	walk = func(ids []string) {
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			if dep := byID[id]; dep != nil {
				walk(dep.DependsOn)
			}
		}
	}
	walk(s.DependsOn)
	return seen
}

// findCycle returns a step id on a depends_on cycle, or "".
func findCycle(steps []Step) string {
	byID := map[string]*Step{}
	for _, s := range allSteps(steps) {
		byID[s.ID] = s
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(id string) string
	visit = func(id string) string {
		switch color[id] {
		case grey:
			return id
		case black:
			return ""
		}
		color[id] = grey
		if s := byID[id]; s != nil {
			for _, d := range s.DependsOn {
				if c := visit(d); c != "" {
					return c
				}
			}
		}
		color[id] = black
		return ""
	}
	for id := range byID {
		if c := visit(id); c != "" {
			return c
		}
	}
	return ""
}
