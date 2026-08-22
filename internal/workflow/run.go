package workflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type RunOptions struct {
	Root   string
	Name   string // workflow name (file .seed/workflows/<name>.yaml)
	Inputs map[string]string
	Mock   bool   // zero side effects: AI steps → mock harness, run: steps recorded, gates auto-pass
	Resume string // run id to resume
}

type StepReport struct {
	Status      string `json:"status"` // success | failed | skipped | paused
	Exit        int    `json:"exit"`
	RecordedCmd string `json:"recorded_cmd,omitempty"` // mock mode: the command NOT executed
	Note        string `json:"note,omitempty"`
	Revisions   int    `json:"revisions,omitempty"`
}

type checkpoint struct {
	RunID    string                 `json:"run_id"`
	Workflow string                 `json:"workflow"`
	DefHash  string                 `json:"def_hash"`
	Status   string                 `json:"status"` // running | succeeded | failed | paused
	Steps    map[string]*StepReport `json:"steps"`
	Outputs  map[string]string      `json:"outputs"` // artifact name -> absolute path
	Pending  string                 `json:"pending_gate,omitempty"`
	Inputs   map[string]string      `json:"inputs"`
}

type RunResult struct {
	RunID     string                 `json:"run_id"`
	Status    string                 `json:"status"`
	RunDir    string                 `json:"run_dir"`
	Steps     map[string]*StepReport `json:"steps"`
	Notes     []string               `json:"notes"`
	NextSteps []string               `json:"next_steps"`
}

type runner struct {
	opts RunOptions
	wf   *Workflow
	dir  string // run dir
	mu   sync.Mutex
	cp   *checkpoint
	res  *RunResult
	ctx  context.Context
}

// Run executes (or resumes) a workflow. Validation failures refuse the
// run; the returned error carries them.
func Run(opts RunOptions) (*RunResult, error) {
	path := filepath.Join(Dir(opts.Root), opts.Name+".yaml")
	if findings := Validate(opts.Root, path, false); len(findings) > 0 {
		return nil, fmt.Errorf("workflow %s fails validation: %s", opts.Name, findings[0])
	}
	wf, err := Load(path)
	if err != nil {
		return nil, err
	}
	for _, in := range wf.Inputs {
		if in.Required {
			if _, ok := opts.Inputs[in.Name]; !ok {
				return nil, fmt.Errorf("required input %q missing (--input %s=...)", in.Name, in.Name)
			}
		}
	}

	base, err := runsBase(opts.Root)
	if err != nil {
		return nil, err
	}
	defHash := defHash(wf, opts.Inputs)

	r := &runner{opts: opts, wf: wf}
	if opts.Resume != "" {
		r.dir = filepath.Join(base, opts.Resume)
		cp, err := loadCheckpoint(r.dir)
		if err != nil {
			return nil, fmt.Errorf("no resumable run %q: %v", opts.Resume, err)
		}
		if cp.Status == "succeeded" {
			return nil, fmt.Errorf("run %s already succeeded — resuming a finished run is refused", opts.Resume)
		}
		if cp.DefHash != defHash {
			return nil, fmt.Errorf("run %s was recorded against a different workflow definition or inputs — a changed workflow starts a fresh run (mixed-graph results are refused)", opts.Resume)
		}
		// Failed steps re-run; completed results survive.
		for id, sr := range cp.Steps {
			if sr.Status == "failed" || sr.Status == "paused" {
				delete(cp.Steps, id)
			}
		}
		cp.Status = "running"
		cp.Pending = ""
		r.cp = cp
	} else {
		id := "wf-" + randHex(6)
		r.dir = filepath.Join(base, id)
		for _, d := range []string{"artifacts", "gates", "logs"} {
			if err := os.MkdirAll(filepath.Join(r.dir, d), 0o755); err != nil {
				return nil, err
			}
		}
		r.cp = &checkpoint{RunID: id, Workflow: wf.Name, DefHash: defHash, Status: "running",
			Steps: map[string]*StepReport{}, Outputs: map[string]string{}, Inputs: opts.Inputs}
	}
	r.res = &RunResult{RunID: r.cp.RunID, RunDir: r.dir, Steps: r.cp.Steps}

	timeout := time.Duration(wf.Budgets.MaxWallClockMinutes) * time.Minute
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	r.ctx = ctx

	r.schedule()

	switch {
	case r.cp.Pending != "":
		r.cp.Status = "paused"
		r.res.NextSteps = append(r.res.NextSteps,
			fmt.Sprintf("approve by writing %s, then rerun with --resume %s", filepath.Join(r.dir, "gates", r.cp.Pending+".response.json"), r.cp.RunID))
	case anyFailed(r.cp.Steps):
		r.cp.Status = "failed"
	default:
		r.cp.Status = "succeeded"
	}
	r.res.Status = r.cp.Status
	if r.opts.Mock {
		r.res.Notes = append(r.res.Notes, "mock run: zero credentials, zero side effects — AI steps served by scripts/harness/mock, run: commands recorded but never executed, gates auto-passed")
	}
	if err := r.save(); err != nil {
		return nil, err
	}
	return r.res, nil
}

// schedule executes the top-level steps (remediation steps excluded — they
// are gate-driven) in topological parallel waves.
func (r *runner) schedule() {
	remediation := remediationIDs(r.wf)
	var pending []*Step
	for i := range r.wf.Steps {
		s := &r.wf.Steps[i]
		if remediation[s.ID] {
			continue
		}
		if sr := r.cp.Steps[s.ID]; sr != nil && (sr.Status == "success" || sr.Status == "skipped") {
			continue // resume: completed steps survive
		}
		pending = append(pending, s)
	}
	for len(pending) > 0 && r.cp.Pending == "" && r.ctx.Err() == nil {
		var wave []*Step
		var rest []*Step
		for _, s := range pending {
			if r.depsSettled(s) {
				wave = append(wave, s)
			} else {
				rest = append(rest, s)
			}
		}
		if len(wave) == 0 {
			// Remaining steps are unreachable (blocked deps): mark skipped.
			for _, s := range rest {
				r.setStep(s.ID, &StepReport{Status: "skipped", Note: "upstream failure blocked this step"})
			}
			return
		}
		var wg sync.WaitGroup
		for _, s := range wave {
			wg.Add(1)
			go func(s *Step) {
				defer wg.Done()
				r.execStep(s)
			}(s)
		}
		wg.Wait()
		pending = rest
	}
}

// depsSettled reports whether every dependency has finished; the
// trigger_rule decides at exec time whether the step runs or skips.
func (r *runner) depsSettled(s *Step) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range s.DependsOn {
		sr := r.cp.Steps[d]
		if sr == nil || (sr.Status != "success" && sr.Status != "failed" && sr.Status != "skipped") {
			return false
		}
	}
	return true
}

func (r *runner) execStep(s *Step) {
	// trigger_rule fan-in arbitration.
	rule := s.TriggerRule
	if rule == "" {
		rule = "all_success"
	}
	ok, failedDep := r.triggerOK(s, rule)
	if !ok {
		r.setStep(s.ID, &StepReport{Status: "skipped", Note: "trigger_rule " + rule + " not met (dep " + failedDep + ")"})
		return
	}
	// when-expression.
	if s.When != "" && !r.evalWhen(s.When) {
		r.setStep(s.ID, &StepReport{Status: "skipped", Note: "when: " + s.When})
		return
	}
	// The gate guards the step: it runs BEFORE the step's own action.
	if s.Gate != nil {
		sr, pauseID := r.runGate(s)
		if pauseID != "" {
			r.mu.Lock()
			r.cp.Pending = pauseID
			r.cp.Steps[s.ID] = sr
			r.mu.Unlock()
			return
		}
		if sr.Status == "failed" {
			r.setStep(s.ID, sr)
			return
		}
		if s.Prompt == "" && s.PromptFile == "" && s.Run == "" && len(s.Steps) == 0 {
			sr.Status = "success"
			r.setStep(s.ID, sr)
			return
		}
		defer func() { // fold gate notes into the final report
			r.mu.Lock()
			if cur := r.cp.Steps[s.ID]; cur != nil && sr.Note != "" {
				cur.Note = strings.TrimPrefix(cur.Note+"; "+sr.Note, "; ")
				cur.Revisions = sr.Revisions
			}
			r.mu.Unlock()
		}()
	}
	if len(s.Steps) > 0 {
		r.execLoop(s)
		return
	}
	r.execAction(s)
}

func (r *runner) triggerOK(s *Step, rule string) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(s.DependsOn) == 0 {
		return true, ""
	}
	success, done := 0, 0
	worst := ""
	for _, d := range s.DependsOn {
		sr := r.cp.Steps[d]
		if sr == nil {
			return false, d
		}
		done++
		if sr.Status == "success" {
			success++
		} else {
			worst = d
		}
	}
	switch rule {
	case "one_success":
		return success >= 1, worst
	case "all_done":
		return done == len(s.DependsOn), worst
	default: // all_success
		return success == len(s.DependsOn), worst
	}
}

// execLoop runs a loop group: body steps in declared order, then the
// until command (exit 0 = done), up to max_iterations. Mock runs execute
// one recorded iteration.
func (r *runner) execLoop(s *Step) {
	iters := s.MaxIterations
	if r.opts.Mock {
		iters = 1
	}
	for i := 0; i < iters; i++ {
		for j := range s.Steps {
			body := &s.Steps[j]
			r.execAction(body)
			r.mu.Lock()
			failed := r.cp.Steps[body.ID] != nil && r.cp.Steps[body.ID].Status == "failed"
			r.mu.Unlock()
			if failed {
				r.setStep(s.ID, &StepReport{Status: "failed", Note: "loop body step " + body.ID + " failed"})
				return
			}
		}
		if r.opts.Mock {
			r.setStep(s.ID, &StepReport{Status: "success", Note: "mock: one recorded iteration", RecordedCmd: s.Until})
			return
		}
		exit := r.shell(s.Until, s.ID+".until")
		if exit == 0 {
			r.setStep(s.ID, &StepReport{Status: "success", Note: fmt.Sprintf("until satisfied after %d iteration(s)", i+1)})
			return
		}
	}
	r.setStep(s.ID, &StepReport{Status: "failed", Note: fmt.Sprintf("max_iterations (%d) reached without until", s.MaxIterations)})
}

// execAction runs the step's own action with budgeted retries, then
// enforces produces + output_format.
func (r *runner) execAction(s *Step) {
	retries := r.wf.Budgets.MaxStepRetries
	var sr *StepReport
	for attempt := 0; ; attempt++ {
		sr = r.attempt(s)
		if sr.Status == "success" || attempt >= retries {
			break
		}
	}
	if sr.Status == "success" {
		if err := r.enforceProduces(s); err != nil {
			sr = &StepReport{Status: "failed", Note: err.Error()}
		}
	}
	if sr.Status == "failed" && s.OnFail == "continue" {
		sr.Note = strings.TrimPrefix(sr.Note+"; on_fail: continue", "; ")
	}
	r.setStep(s.ID, sr)
}

func (r *runner) attempt(s *Step) *StepReport {
	switch {
	case s.Run != "":
		if r.opts.Mock {
			// Mock purity: the command is recorded, never executed; its
			// produces are materialized as stubs.
			if err := r.stubProduces(s); err != nil {
				return &StepReport{Status: "failed", Note: err.Error()}
			}
			return &StepReport{Status: "success", RecordedCmd: r.subst(s.Run), Note: "mock: command recorded, not executed"}
		}
		exit := r.shell(r.subst(s.Run), s.ID)
		if exit != 0 {
			return &StepReport{Status: "failed", Exit: exit, Note: "run command failed"}
		}
		return &StepReport{Status: "success"}
	default:
		return r.harness(s)
	}
}

// harness invokes scripts/seed-harness <name> per the adapter contract:
// prompt on stdin, one JSON envelope on stdout, SEED_* env. tools maps to
// SEED_PERMISSION (readonly → read-only, coding → safe-edit); no workflow
// value maps to yolo.
func (r *runner) harness(s *Step) *StepReport {
	prompt := s.Prompt
	if s.PromptFile != "" {
		raw, err := os.ReadFile(filepath.Join(Dir(r.opts.Root), s.PromptFile))
		if err != nil {
			return &StepReport{Status: "failed", Note: err.Error()}
		}
		prompt = string(raw)
	}
	prompt = r.subst(prompt)
	name := firstNonEmpty(s.Harness, r.wf.Defaults.Harness)
	if r.opts.Mock {
		name = "mock"
	}
	perm := "safe-edit"
	if s.Tools == "readonly" {
		perm = "read-only"
	}
	produces, err := r.producesEnv(s)
	if err != nil {
		return &StepReport{Status: "failed", Note: err.Error()}
	}
	cmd := exec.CommandContext(r.ctx, filepath.Join(r.opts.Root, "scripts", "seed-harness"), name)
	cmd.Dir = r.opts.Root
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(),
		"SEED_ROLE="+s.Role,
		"SEED_PERMISSION="+perm,
		"SEED_MODEL="+firstNonEmpty(s.Model, r.wf.Defaults.Model),
		"SEED_STEP="+s.ID,
		"SEED_RUN_DIR="+r.dir,
		"SEED_PRODUCES="+produces,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	_ = os.WriteFile(filepath.Join(r.dir, "logs", s.ID+".log"), out.Bytes(), 0o644)
	if err != nil {
		exit := 1
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		}
		return &StepReport{Status: "failed", Exit: exit, Note: "harness " + name + " failed"}
	}
	return &StepReport{Status: "success"}
}

// enforceProduces checks declared artifacts exist, registers their paths
// for {{output...}} tokens, and validates JSON artifacts against their
// per-produce schema or the step's output_format (which applies to the
// step's first produce).
func (r *runner) enforceProduces(s *Step) error {
	for i, p := range s.Produces {
		abs := filepath.Join(r.dir, p.File)
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("step %s did not produce %s (%s)", s.ID, p.Name, p.File)
		}
		var schema map[string]any
		if p.Schema != "" {
			raw, err := os.ReadFile(filepath.Join(Dir(r.opts.Root), p.Schema))
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				return fmt.Errorf("schema %s: %v", p.Schema, err)
			}
		} else if i == 0 && s.OutputFormat != nil {
			schema = s.OutputFormat
		}
		if schema != nil {
			if err := validateJSON(abs, schema); err != nil {
				return fmt.Errorf("step %s produce %s violates its schema: %v", s.ID, p.Name, err)
			}
		}
		r.mu.Lock()
		r.cp.Outputs[p.Name] = abs
		r.mu.Unlock()
	}
	return nil
}

func validateJSON(path string, schema map[string]any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	blob, _ := json.Marshal(schema)
	sch, err := jsonschema.UnmarshalJSON(bytes.NewReader(blob))
	if err != nil {
		return err
	}
	if err := c.AddResource("inline.json", sch); err != nil {
		return err
	}
	compiled, err := c.Compile("inline.json")
	if err != nil {
		return err
	}
	return compiled.Validate(doc)
}

// stubProduces materializes schema-valid stubs (mock run: steps).
func (r *runner) stubProduces(s *Step) error {
	for i, p := range s.Produces {
		abs := filepath.Join(r.dir, p.File)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		var schema map[string]any
		if p.Schema != "" {
			raw, err := os.ReadFile(filepath.Join(Dir(r.opts.Root), p.Schema))
			if err != nil {
				return err
			}
			_ = json.Unmarshal(raw, &schema)
		} else if i == 0 && s.OutputFormat != nil {
			schema = s.OutputFormat
		}
		var content []byte
		if schema != nil || strings.HasSuffix(p.File, ".json") {
			content, _ = json.MarshalIndent(genStub(schema), "", "  ")
		} else {
			content = []byte("mock artifact " + p.Name + "\n")
		}
		if err := os.WriteFile(abs, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// genStub builds a minimal instance satisfying a (simple) JSON Schema:
// required properties get zero values by type.
func genStub(schema map[string]any) any {
	if schema == nil {
		return map[string]any{}
	}
	t, _ := schema["type"].(string)
	switch t {
	case "string":
		if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
			return enum[0]
		}
		return "mock"
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	default: // object or unspecified
		out := map[string]any{}
		props, _ := schema["properties"].(map[string]any)
		req, _ := schema["required"].([]any)
		for _, k := range req {
			name, _ := k.(string)
			sub, _ := props[name].(map[string]any)
			out[name] = genStub(sub)
		}
		return out
	}
}

// subst resolves {{inputs.*}} and {{output.<name>.path}} tokens.
func (r *runner) subst(text string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return tokenRe.ReplaceAllStringFunc(text, func(m string) string {
		g := tokenRe.FindStringSubmatch(m)
		switch g[1] {
		case "inputs":
			return r.cp.Inputs[g[2]]
		case "output":
			if g[3] == ".path" || g[3] == "" {
				return r.cp.Outputs[g[2]]
			}
		}
		return m
	})
}

var whenRe = regexp.MustCompile(`^\s*(steps\.([A-Za-z0-9_-]+)\.outcome|inputs\.([A-Za-z0-9_-]+))\s*(==|!=)\s*'([^']*)'\s*$`)

// evalWhen supports the documented minimal expression grammar:
// steps.<id>.outcome == 'success' and inputs.<name> == 'value' (and !=).
// An unparseable expression is conservative: the step is skipped.
func (r *runner) evalWhen(expr string) bool {
	m := whenRe.FindStringSubmatch(expr)
	if m == nil {
		return false
	}
	r.mu.Lock()
	var actual string
	if m[2] != "" {
		if sr := r.cp.Steps[m[2]]; sr != nil {
			actual = sr.Status
		}
	} else {
		actual = r.cp.Inputs[m[3]]
	}
	r.mu.Unlock()
	if m[4] == "==" {
		return actual == m[5]
	}
	return actual != m[5]
}

func (r *runner) shell(cmdline, logName string) int {
	cmd := exec.CommandContext(r.ctx, "sh", "-c", cmdline)
	cmd.Dir = r.opts.Root
	cmd.Env = append(os.Environ(), "SEED_RUN_DIR="+r.dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	_ = os.WriteFile(filepath.Join(r.dir, "logs", logName+".log"), out.Bytes(), 0o644)
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

func (r *runner) producesEnv(s *Step) (string, error) {
	type entry struct {
		Name   string         `json:"name"`
		File   string         `json:"file"`
		Schema map[string]any `json:"schema,omitempty"`
	}
	var list []entry
	for i, p := range s.Produces {
		abs := filepath.Join(r.dir, p.File)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", err
		}
		e := entry{Name: p.Name, File: abs}
		if p.Schema != "" {
			raw, err := os.ReadFile(filepath.Join(Dir(r.opts.Root), p.Schema))
			if err != nil {
				return "", err
			}
			_ = json.Unmarshal(raw, &e.Schema)
		} else if i == 0 && s.OutputFormat != nil {
			e.Schema = s.OutputFormat
		}
		list = append(list, e)
	}
	b, err := json.Marshal(list)
	return string(b), err
}

func (r *runner) setStep(id string, sr *StepReport) {
	r.mu.Lock()
	r.cp.Steps[id] = sr
	r.mu.Unlock()
	_ = r.save()
}

func (r *runner) save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.MarshalIndent(r.cp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dir, "checkpoint.json"), b, 0o644)
}

func loadCheckpoint(dir string) (*checkpoint, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		return nil, err
	}
	var cp checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, err
	}
	if cp.Steps == nil {
		cp.Steps = map[string]*StepReport{}
	}
	if cp.Outputs == nil {
		cp.Outputs = map[string]string{}
	}
	return &cp, nil
}

func anyFailed(steps map[string]*StepReport) bool {
	for _, sr := range steps {
		if sr.Status == "failed" && !strings.Contains(sr.Note, "on_fail: continue") {
			return true
		}
	}
	return false
}

// runsBase resolves <git-common-dir>/seed-runs — local, shared across
// linked worktrees, invisible to commits and CI (the fastcards placement
// precedent). SEED_RUNS_DIR overrides for tests.
func runsBase(root string) (string, error) {
	if v := os.Getenv("SEED_RUNS_DIR"); v != "" {
		return v, nil
	}
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %v", err)
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	return filepath.Join(common, "seed-runs"), nil
}

// defHash binds a checkpoint to the fully resolved workflow definition +
// inputs: a mismatched resume is refused (no mixed-graph results).
func defHash(w *Workflow, inputs map[string]string) string {
	h := sha256.New()
	h.Write(w.raw)
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%s", k, inputs[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
