package workflow

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dumpSteps(steps map[string]*StepReport) string {
	var b strings.Builder
	for id, sr := range steps {
		fmt.Fprintf(&b, "%s=%+v ", id, *sr)
	}
	return b.String()
}

// A non-mock run through the fake harness: AI step with produces +
// output_format, {{output...}} substitution into a run: step, when
// expressions, and a loop that satisfies its until command.
func TestRealRunHappyPath(t *testing.T) {
	root := mkroot(t)
	if err := os.MkdirAll(filepath.Join(Dir(root), "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(root), "prompts", "p.md"), []byte("do the thing"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWF(t, root, "real", hdr("real")+`
inputs:
  - {name: flag, type: string, required: true}
steps:
  - id: plan
    role: planner
    tools: readonly
    prompt: "plan {{inputs.flag}}"
    produces:
      - {name: plan, file: artifacts/plan.json}
    output_format:
      type: object
      required: [ok]
      properties:
        ok: {enum: ["good"]}
  - id: pf
    role: implementer
    prompt_file: prompts/p.md
    depends_on: [plan]
    produces:
      - {name: pfout, file: artifacts/pf.txt}
  - id: use
    run: "test -s '{{output.plan.path}}' && echo used {{inputs.flag}}"
    depends_on: [plan]
  - id: whenskip
    run: "true"
    when: "steps.use.outcome == 'failed'"
    depends_on: [use]
  - id: whenrun
    run: "true"
    when: "inputs.flag != 'nope'"
    depends_on: [use]
  - id: badwhen
    run: "true"
    when: "this is not the grammar"
    depends_on: [use]
  - id: finish
    depends_on: [use]
    max_iterations: 3
    until: "test -f \"$SEED_RUN_DIR/artifacts/done\""
    steps:
      - id: finish-body
        run: "touch \"$SEED_RUN_DIR/artifacts/done\""
`)
	// A missing required input refuses the run before anything executes.
	if _, err := Run(RunOptions{Root: root, Name: "real"}); err == nil ||
		!strings.Contains(err.Error(), "required input") {
		t.Fatalf("missing input tolerated: %v", err)
	}
	res, err := Run(RunOptions{Root: root, Name: "real", Inputs: map[string]string{"flag": "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("run: %s", dumpSteps(res.Steps))
	}
	for id, want := range map[string]string{
		"plan": "success", "pf": "success", "use": "success",
		"whenskip": "skipped", "whenrun": "success", "badwhen": "skipped",
		"finish": "success",
	} {
		if res.Steps[id] == nil || res.Steps[id].Status != want {
			t.Fatalf("step %s: %+v", id, res.Steps[id])
		}
	}
	if !strings.Contains(res.Steps["finish"].Note, "until satisfied") {
		t.Fatalf("loop note: %q", res.Steps["finish"].Note)
	}
	envLog, _ := os.ReadFile(filepath.Join(res.RunDir, "harness-env.log"))
	if !strings.Contains(string(envLog), "role=planner perm=read-only") {
		t.Fatalf("harness env: %s", envLog)
	}
}

// Failure propagation: retries, on_fail: continue, trigger rules, loop
// failure modes, and a produce the step never wrote.
func TestRealRunFailurePropagation(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "grim", hdr("grim")+`
budgets: {max_step_retries: 1}
steps:
  - id: boom
    run: "exit 3"
    on_fail: continue
  - id: ok
    run: "true"
  - id: after-default
    run: "true"
    depends_on: [boom]
  - id: after-alldone
    run: "true"
    trigger_rule: all_done
    depends_on: [boom]
  - id: after-onesucc
    run: "true"
    trigger_rule: one_success
    depends_on: [boom, ok]
  - id: badloop
    depends_on: [ok]
    max_iterations: 2
    until: "false"
    steps:
      - id: badloop-body
        run: "true"
  - id: failbody
    depends_on: [ok]
    max_iterations: 1
    until: "true"
    steps:
      - id: failbody-body
        run: "exit 1"
  - id: ghost
    run: "true"
    produces:
      - {name: g, file: artifacts/g.txt}
`)
	res, err := Run(RunOptions{Root: root, Name: "grim"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatalf("status: %s", res.Status)
	}
	if n := res.Steps["boom"].Note; !strings.Contains(n, "on_fail: continue") {
		t.Fatalf("boom: %+v", res.Steps["boom"])
	}
	if res.Steps["after-default"].Status != "skipped" ||
		res.Steps["after-alldone"].Status != "success" ||
		res.Steps["after-onesucc"].Status != "success" {
		t.Fatalf("trigger rules: default=%+v alldone=%+v onesucc=%+v",
			res.Steps["after-default"], res.Steps["after-alldone"], res.Steps["after-onesucc"])
	}
	if n := res.Steps["badloop"].Note; !strings.Contains(n, "max_iterations") {
		t.Fatalf("badloop: %q", n)
	}
	if n := res.Steps["failbody"].Note; !strings.Contains(n, "loop body step") {
		t.Fatalf("failbody: %q", n)
	}
	if n := res.Steps["ghost"].Note; !strings.Contains(n, "did not produce") {
		t.Fatalf("ghost: %q", n)
	}
}

// Approval gate: first run pauses, a written response file lets a resume
// pass the gate and run the guarded action.
func TestApprovalGatePauseAndResume(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "gated", hdr("gated")+`
steps:
  - id: prep
    run: "true"
  - id: ship
    run: "true"
    depends_on: [prep]
    gate:
      type: approval
      message: "sign off"
      capture_response: true
`)
	res, err := Run(RunOptions{Root: root, Name: "gated"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "paused" || res.Steps["ship"].Note != "sign off" {
		t.Fatalf("pause: %s %+v", res.Status, res.Steps["ship"])
	}
	if len(res.NextSteps) == 0 || !strings.Contains(res.NextSteps[0], "--resume") {
		t.Fatalf("next steps: %v", res.NextSteps)
	}
	// A resume against different inputs is a different definition.
	if _, err := Run(RunOptions{Root: root, Name: "gated", Resume: res.RunID,
		Inputs: map[string]string{"x": "y"}}); err == nil ||
		!strings.Contains(err.Error(), "different workflow definition") {
		t.Fatalf("defhash drift tolerated: %v", err)
	}
	resp := filepath.Join(res.RunDir, "gates", "ship.response.json")
	if err := os.WriteFile(resp, []byte(`{"approved": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := Run(RunOptions{Root: root, Name: "gated", Resume: res.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != "succeeded" || !strings.Contains(res2.Steps["ship"].Note, "response captured") {
		t.Fatalf("resume: %s %+v", res2.Status, res2.Steps["ship"])
	}
}

// Review gate: straight APPROVE leaves the remediation step unexecuted
// (its dependents are unreachable), REVISE runs it before re-review.
func TestReviewGateVerdicts(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "reviewed", hdr("reviewed")+`
steps:
  - id: impl
    run: "true"
    gate:
      type: review
      reviewer_role: reviewer
      remediation: fix
      max_revisions: 2
  - id: fix
    run: "true"
  - id: stuck
    run: "true"
    depends_on: [fix]
`)
	// No FAKE_REVIEW_SEQ: the fake harness pins the first enum, APPROVE.
	res, err := Run(RunOptions{Root: root, Name: "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" || res.Steps["impl"].Revisions != 0 {
		t.Fatalf("approve path: %s %+v", res.Status, res.Steps["impl"])
	}
	if res.Steps["stuck"].Status != "skipped" {
		t.Fatalf("unreachable dependent: %+v", res.Steps["stuck"])
	}

	// REVISE once, then APPROVE: the remediation runs in between.
	t.Setenv("FAKE_REVIEW_SEQ", filepath.Join(t.TempDir(), "seq"))
	res2, err := Run(RunOptions{Root: root, Name: "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != "succeeded" || res2.Steps["impl"].Revisions != 1 {
		t.Fatalf("revise path: %s %+v", res2.Status, res2.Steps["impl"])
	}
	if res2.Steps["fix"] == nil || res2.Steps["fix"].Status != "success" ||
		res2.Steps["stuck"].Status != "success" {
		t.Fatalf("remediation: fix=%+v stuck=%+v", res2.Steps["fix"], res2.Steps["stuck"])
	}
}

func reviewWF(t *testing.T, root, name, remediationRun string) {
	t.Helper()
	writeWF(t, root, name, hdr(name)+`
steps:
  - id: impl
    run: "true"
    gate:
      type: review
      reviewer_role: reviewer
      remediation: fix
      max_revisions: 1
  - id: fix
    run: "`+remediationRun+`"
`)
}

func TestReviewGateFailureModes(t *testing.T) {
	// A reviewer that never approves exhausts max_revisions.
	always := `#!/bin/sh
cat > /dev/null
mkdir -p "$SEED_RUN_DIR/gates"
echo '{"verdict": "REVISE"}' > "$SEED_RUN_DIR/gates/impl.review.json"
`
	root := mkroot(t)
	if err := os.WriteFile(filepath.Join(root, "scripts", "harness", "fake"), []byte(always), 0o755); err != nil {
		t.Fatal(err)
	}
	reviewWF(t, root, "loopy", "true")
	res, err := Run(RunOptions{Root: root, Name: "loopy"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || !strings.Contains(res.Steps["impl"].Note, "still REVISE") {
		t.Fatalf("revise cap: %+v", res.Steps["impl"])
	}

	// REVISE with a remediation that fails.
	root2 := mkroot(t)
	if err := os.WriteFile(filepath.Join(root2, "scripts", "harness", "fake"), []byte(always), 0o755); err != nil {
		t.Fatal(err)
	}
	reviewWF(t, root2, "broken", "exit 1")
	res, err = Run(RunOptions{Root: root2, Name: "broken"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Steps["impl"].Note, "remediation step fix failed") {
		t.Fatalf("remediation failure: %+v", res.Steps["impl"])
	}

	// A verdict outside the contract fails the gate.
	root3 := mkroot(t)
	garbage := strings.Replace(always, "REVISE", "SHRUG", 1)
	if err := os.WriteFile(filepath.Join(root3, "scripts", "harness", "fake"), []byte(garbage), 0o755); err != nil {
		t.Fatal(err)
	}
	reviewWF(t, root3, "vague", "true")
	res, err = Run(RunOptions{Root: root3, Name: "vague"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Steps["impl"].Note, "no APPROVE|APPROVE_WITH_NOTES|REVISE") {
		t.Fatalf("garbage verdict: %+v", res.Steps["impl"])
	}

	// A reviewer harness that dies, and one that writes no verdict file.
	root4 := mkroot(t)
	if err := os.WriteFile(filepath.Join(root4, "scripts", "harness", "fake"), []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	reviewWF(t, root4, "dead", "true")
	res, err = Run(RunOptions{Root: root4, Name: "dead"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Steps["impl"].Note, "reviewer harness failed") {
		t.Fatalf("dead harness: %+v", res.Steps["impl"])
	}
	root5 := mkroot(t)
	if err := os.WriteFile(filepath.Join(root5, "scripts", "harness", "fake"), []byte("#!/bin/sh\ncat > /dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	reviewWF(t, root5, "mute", "true")
	res, err = Run(RunOptions{Root: root5, Name: "mute"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Steps["impl"].Note, "no verdict file") {
		t.Fatalf("mute harness: %+v", res.Steps["impl"])
	}
}

func checksWF(t *testing.T, root, name, gateBody string) {
	t.Helper()
	writeWF(t, root, name, hdr(name)+`
steps:
  - id: chk
    gate:
      type: checks
`+gateBody)
}

func TestChecksGateBranches(t *testing.T) {
	root := mkroot(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"check_runs": [{"name": "ci", "status": "completed", "conclusion": "success"}]}`))
	}))
	defer api.Close()
	gql := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": {"repository": {"pullRequest": {"reviewThreads": {"nodes": [{"isResolved": true}]}}}}}`))
	}))
	defer gql.Close()
	t.Setenv("SEED_GITHUB_API", api.URL)
	t.Setenv("SEED_GITHUB_GRAPHQL", gql.URL)
	t.Setenv("GITHUB_TOKEN", "tok")

	checksWF(t, root, "green", `      required_ci: true
      unresolved_threads: 0
      repo: "o/r"
      ref: "main"
      pr: "7"
`)
	res, err := Run(RunOptions{Root: root, Name: "green"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" || !strings.Contains(res.Steps["chk"].Note, "checks gate passed") {
		t.Fatalf("green: %s %+v", res.Status, res.Steps["chk"])
	}

	// A red check closes the gate.
	red := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"check_runs": [{"name": "ci", "status": "completed", "conclusion": "failure"}]}`))
	}))
	defer red.Close()
	t.Setenv("SEED_GITHUB_API", red.URL)
	res, _ = Run(RunOptions{Root: root, Name: "green"})
	if res.Status != "failed" || !strings.Contains(res.Steps["chk"].Note, "checks gate closed: check ci") {
		t.Fatalf("red: %+v", res.Steps["chk"])
	}

	// An unresolved review thread closes it too.
	t.Setenv("SEED_GITHUB_API", api.URL)
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": {"repository": {"pullRequest": {"reviewThreads": {"nodes": [{"isResolved": false}]}}}}}`))
	}))
	defer open.Close()
	t.Setenv("SEED_GITHUB_GRAPHQL", open.URL)
	res, _ = Run(RunOptions{Root: root, Name: "green"})
	if !strings.Contains(res.Steps["chk"].Note, "unresolved review thread") {
		t.Fatalf("threads: %+v", res.Steps["chk"])
	}

	// HTTP errors surface with the status code.
	boom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer boom.Close()
	t.Setenv("SEED_GITHUB_API", boom.URL)
	res, _ = Run(RunOptions{Root: root, Name: "green"})
	if !strings.Contains(res.Steps["chk"].Note, "HTTP 500") {
		t.Fatalf("http error: %+v", res.Steps["chk"])
	}

	// repo must be owner/name when threads are checked.
	checksWF(t, root, "badrepo", `      unresolved_threads: 0
      repo: "nogood"
      pr: "7"
`)
	res, _ = Run(RunOptions{Root: root, Name: "badrepo"})
	if !strings.Contains(res.Steps["chk"].Note, "owner/name") {
		t.Fatalf("bad repo: %+v", res.Steps["chk"])
	}

	// No token, no gate.
	t.Setenv("GITHUB_TOKEN", "")
	res, _ = Run(RunOptions{Root: root, Name: "green"})
	if !strings.Contains(res.Steps["chk"].Note, "GITHUB_TOKEN") {
		t.Fatalf("token: %+v", res.Steps["chk"])
	}
}

// One workflow tripping most validation rules at once.
func TestValidateRuleMatrix(t *testing.T) {
	root := mkroot(t)
	path := writeWF(t, root, "mess", `schema_version: "2"
name: wrong-name
description: broken on purpose
steps:
  - id: a
    run: "true"
    depends_on: [b, nowhere]
    tools: sudo
    trigger_rule: sometimes
    on_fail: shrug
  - id: b
    run: "true"
    depends_on: [a]
  - id: ""
    prompt: x
  - id: Bad_Case
    run: "true"
  - id: a
    run: "true"
  - id: twoact
    prompt: x
    run: "true"
  - id: loopact
    run: "true"
    max_iterations: 0
    steps:
      - id: loopact-body
        run: "true"
  - id: noact
  - id: ghostschema
    run: "true"
    produces:
      - {name: "", file: ""}
      - {name: art, file: artifacts/a.json, schema: schemas/none.json}
  - id: eater
    run: "true"
    consumes: [nothing, art]
  - id: roleless
    role: nobody
    prompt: x
    gate:
      type: review
      reviewer_role: phantom
  - id: badgate
    run: "true"
    gate:
      type: vibes
  - id: fix-dep
    run: "true"
    depends_on: [b]
  - id: gated2
    run: "true"
    gate:
      type: review
      reviewer_role: reviewer
      remediation: fix-dep
  - id: gated3
    run: "true"
    gate:
      type: review
      reviewer_role: reviewer
  - id: exotic
    harness: qbit
    model: t9
    prompt: "{{inputs.ghost}} {{output.ghost.path}} {{steps.ghost.outcome}} {{wat.no}}"
  - id: dangling
    run: "true"
    until: "false"
`)
	fs := Validate(root, path, false)
	for _, rule := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12} {
		if !hasRule(fs, rule) {
			t.Errorf("rule %d not tripped; findings: %v", rule, fs)
		}
	}

	// Registry and role closure read errors.
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, ".seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, ".seed", "config.toml"), []byte("[[["), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, ".seed", "agents"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2 := writeWF(t, bad, "tiny", hdr("tiny")+"steps:\n  - id: s\n    run: \"true\"\n")
	fs = Validate(bad, p2, false)
	if !hasRule(fs, 9) || !hasRule(fs, 8) {
		t.Fatalf("registry/roles read errors not surfaced: %v", fs)
	}
}
