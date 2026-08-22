package workflow

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The harness fixture records its env and materializes SEED_PRODUCES as
// schema-aware stubs (enum[0] wins), standing in for both the template's
// mock harness and a live adapter. FAKE_REVIEW_SEQ makes the reviewer
// return REVISE on the first call, APPROVE after.
const fakeHarness = `#!/bin/sh
set -eu
cat > /dev/null
echo "role=$SEED_ROLE perm=$SEED_PERMISSION model=$SEED_MODEL step=$SEED_STEP" >> "$SEED_RUN_DIR/harness-env.log"
python3 - <<'EOF'
import json, os
seq = os.environ.get("FAKE_REVIEW_SEQ")
for e in json.loads(os.environ.get("SEED_PRODUCES") or "[]"):
    os.makedirs(os.path.dirname(e["file"]), exist_ok=True)
    sch = e.get("schema") or {}
    if sch or e["file"].endswith(".json"):
        out = {}
        props = sch.get("properties", {})
        for k in sch.get("required", []):
            p = props.get(k, {})
            out[k] = (p.get("enum") or ["mock"])[0]
        if seq and e["name"].endswith("-review"):
            n = 0
            if os.path.exists(seq):
                n = int(open(seq).read() or 0)
            out["verdict"] = "REVISE" if n == 0 else "APPROVE"
            open(seq, "w").write(str(n + 1))
        open(e["file"], "w").write(json.dumps(out))
    else:
        open(e["file"], "w").write("fake artifact " + e["name"] + "\n")
EOF
echo '{"result":"ok"}'
`

func mkroot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string, mode os.FileMode) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".seed/config.toml", "[workflows]\nharnesses = [\"fake\", \"mock\", \"claude\"]\nmodels = [\"sonnet\", \"opus\"]\n", 0o644)
	for _, role := range []string{"planner", "implementer", "reviewer"} {
		write(".seed/agents/"+role+".md", "---\nrole: "+role+"\n---\n", 0o644)
	}
	write("scripts/seed-harness", "#!/bin/sh\nexec \"$(dirname \"$0\")/harness/$1\"\n", 0o755)
	write("scripts/harness/fake", fakeHarness, 0o755)
	write("scripts/harness/mock", fakeHarness, 0o755)
	t.Setenv("SEED_RUNS_DIR", t.TempDir())
	return root
}

func writeWF(t *testing.T, root, name, body string) string {
	t.Helper()
	path := filepath.Join(Dir(root), name+".yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const header = "schema_version: \"1\"\nname: %s\ndescription: test\ndefaults: {harness: fake, model: sonnet}\n"

func hdr(name string) string { return fmt.Sprintf(header, name) }

func hasRule(fs []Finding, rule int) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func TestValidateRules(t *testing.T) {
	root := mkroot(t)
	cases := []struct {
		name string
		rule int
		body string
	}{
		{"unknown-field", 1, hdr("unknown-field") + "steps:\n  - {id: a, run: \"true\", bogus_field: 1}\n"},
		{"bad-version", 2, "schema_version: \"9\"\nname: bad-version\nsteps:\n  - {id: a, run: \"true\"}\n"},
		{"dup-ids", 3, hdr("dup-ids") + "steps:\n  - {id: a, run: \"true\"}\n  - {id: a, run: \"false\"}\n"},
		{"bad-kebab", 3, hdr("bad-kebab") + "steps:\n  - {id: NotKebab, run: \"true\"}\n"},
		{"missing-dep", 4, hdr("missing-dep") + "steps:\n  - {id: a, run: \"true\", depends_on: [ghost]}\n"},
		{"cycle", 4, hdr("cycle") + "steps:\n  - {id: a, run: \"true\", depends_on: [b]}\n  - {id: b, run: \"true\", depends_on: [a]}\n"},
		{"two-actions", 5, hdr("two-actions") + "steps:\n  - {id: a, run: \"true\", prompt: also}\n"},
		{"no-action", 5, hdr("no-action") + "steps:\n  - {id: a}\n"},
		{"missing-prompt-file", 6, hdr("missing-prompt-file") + "steps:\n  - {id: a, prompt_file: prompts/nope.md}\n"},
		{"unproduced-consume", 7, hdr("unproduced-consume") + "steps:\n  - {id: a, run: \"true\", consumes: [ghost]}\n"},
		{"unreachable-consume", 7, hdr("unreachable-consume") + "steps:\n  - {id: a, run: \"true\", produces: [{name: x, file: artifacts/x}]}\n  - {id: b, run: \"true\", consumes: [x]}\n"},
		{"bad-role", 8, hdr("bad-role") + "steps:\n  - {id: a, role: ghost, prompt: p}\n"},
		{"bad-harness", 9, hdr("bad-harness") + "steps:\n  - {id: a, harness: ghost, prompt: p}\n"},
		{"bad-model", 9, hdr("bad-model") + "steps:\n  - {id: a, model: gpt-nope, prompt: p}\n"},
		{"bad-budget", 10, hdr("bad-budget") + "budgets: {max_step_retries: -1}\nsteps:\n  - {id: a, run: \"true\"}\n"},
		{"loop-no-until", 11, hdr("loop-no-until") + "steps:\n  - id: a\n    max_iterations: 3\n    steps:\n      - {id: b, run: \"true\"}\n"},
		{"loop-no-max", 11, hdr("loop-no-max") + "steps:\n  - id: a\n    until: \"true\"\n    steps:\n      - {id: b, run: \"true\"}\n"},
		{"bad-token", 12, hdr("bad-token") + "steps:\n  - {id: a, run: \"echo {{inputs.ghost}}\"}\n"},
		{"bad-output-token", 12, hdr("bad-output-token") + "steps:\n  - {id: a, run: \"cat {{output.ghost.path}}\"}\n"},
		{"missing-adapter-13", 13, hdr("missing-adapter-13") + "steps:\n  - {id: a, harness: claude, prompt: p}\n"},
	}
	for _, c := range cases {
		path := writeWF(t, root, c.name, c.body)
		fs := Validate(root, path, c.rule == 13)
		if !hasRule(fs, c.rule) {
			t.Errorf("%s: want rule %d, got %v", c.name, c.rule, fs)
		}
	}
	// A clean workflow validates clean.
	clean := writeWF(t, root, "clean", hdr("clean")+`inputs:
  - {name: goal, type: string, required: false}
steps:
  - id: a
    run: "true"
    produces: [{name: x, file: artifacts/x.txt}]
  - id: b
    role: planner
    tools: readonly
    depends_on: [a]
    consumes: [x]
    prompt: "use {{output.x.path}} for {{inputs.goal}}"
`)
	if fs := Validate(root, clean, false); len(fs) != 0 {
		t.Fatalf("clean workflow has findings: %v", fs)
	}
}

func TestRunWavesTokensAndPermission(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "flow", hdr("flow")+`inputs:
  - {name: goal, type: string, required: true}
steps:
  - id: a
    run: "printf hello > $SEED_RUN_DIR/artifacts/x.txt && touch $SEED_RUN_DIR/a.done"
    produces: [{name: x, file: artifacts/x.txt}]
  - id: par-one
    run: "test -f $SEED_RUN_DIR/a.done && touch $SEED_RUN_DIR/p1"
    depends_on: [a]
  - id: par-two
    run: "test -f $SEED_RUN_DIR/a.done && touch $SEED_RUN_DIR/p2"
    depends_on: [a]
  - id: reader
    role: planner
    tools: readonly
    depends_on: [par-one, par-two]
    consumes: [x]
    prompt: "read {{output.x.path}} for {{inputs.goal}}"
    produces: [{name: note, file: artifacts/note.txt}]
`)
	res, err := Run(RunOptions{Root: root, Name: "flow", Inputs: map[string]string{"goal": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("status %s: %+v", res.Status, res.Steps)
	}
	for _, f := range []string{"p1", "p2", "artifacts/note.txt"} {
		if _, err := os.Stat(filepath.Join(res.RunDir, f)); err != nil {
			t.Fatalf("missing %s", f)
		}
	}
	env, _ := os.ReadFile(filepath.Join(res.RunDir, "harness-env.log"))
	if !strings.Contains(string(env), "role=planner perm=read-only model=sonnet") {
		t.Fatalf("tools→SEED_PERMISSION mapping lost: %s", env)
	}
}

func TestRequiredInputMissing(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "needy", hdr("needy")+"inputs:\n  - {name: x, type: string, required: true}\nsteps:\n  - {id: a, run: \"true\"}\n")
	if _, err := Run(RunOptions{Root: root, Name: "needy"}); err == nil || !strings.Contains(err.Error(), "required input") {
		t.Fatalf("missing input not refused: %v", err)
	}
}

func TestWhenTriggerOnFail(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "branchy", hdr("branchy")+`steps:
  - id: fails
    run: "false"
    on_fail: continue
  - id: skipped-by-rule
    run: "true"
    depends_on: [fails]
  - id: runs-all-done
    run: "touch $SEED_RUN_DIR/all-done-ran"
    depends_on: [fails]
    trigger_rule: all_done
  - id: when-no
    run: "touch $SEED_RUN_DIR/never"
    depends_on: [runs-all-done]
    when: "steps.fails.outcome == 'success'"
  - id: when-yes
    run: "touch $SEED_RUN_DIR/yes"
    depends_on: [runs-all-done]
    when: "steps.fails.outcome != 'success'"
`)
	res, err := Run(RunOptions{Root: root, Name: "branchy"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("on_fail: continue should not fail the run: %+v", res.Steps)
	}
	if res.Steps["skipped-by-rule"].Status != "skipped" {
		t.Fatalf("all_success dep on failed step must skip: %+v", res.Steps["skipped-by-rule"])
	}
	if _, err := os.Stat(filepath.Join(res.RunDir, "all-done-ran")); err != nil {
		t.Fatal("all_done step did not run")
	}
	if _, err := os.Stat(filepath.Join(res.RunDir, "never")); err == nil {
		t.Fatal("when == false step ran")
	}
	if _, err := os.Stat(filepath.Join(res.RunDir, "yes")); err != nil {
		t.Fatal("when != step did not run")
	}
}

func TestLoopBothTerminations(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "looping", hdr("looping")+`steps:
  - id: loop
    until: "test -f $SEED_RUN_DIR/done-flag"
    max_iterations: 3
    steps:
      - id: body
        run: "echo tick >> $SEED_RUN_DIR/ticks && [ $(wc -l < $SEED_RUN_DIR/ticks) -lt 2 ] || touch $SEED_RUN_DIR/done-flag"
`)
	res, err := Run(RunOptions{Root: root, Name: "looping"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" || !strings.Contains(res.Steps["loop"].Note, "after 2") {
		t.Fatalf("until loop: %+v", res.Steps["loop"])
	}

	writeWF(t, root, "endless", hdr("endless")+`steps:
  - id: loop
    until: "false"
    max_iterations: 2
    steps:
      - id: body
        run: "true"
`)
	res, err = Run(RunOptions{Root: root, Name: "endless"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || !strings.Contains(res.Steps["loop"].Note, "max_iterations") {
		t.Fatalf("max_iterations exhaustion: %+v", res.Steps["loop"])
	}
}

func TestOutputFormatEnforced(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "typed", hdr("typed")+`steps:
  - id: a
    run: "printf '{\"wrong\": true}' > $SEED_RUN_DIR/artifacts/v.json"
    output_format: {type: object, required: [verdict], properties: {verdict: {type: string}}}
    produces: [{name: v, file: artifacts/v.json}]
`)
	res, err := Run(RunOptions{Root: root, Name: "typed"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || !strings.Contains(res.Steps["a"].Note, "schema") {
		t.Fatalf("output_format violation not caught: %+v", res.Steps["a"])
	}
}

func TestMockPurity(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "mocked", hdr("mocked")+`steps:
  - id: danger
    run: "touch {{inputs.marker}}"
    produces: [{name: out, file: artifacts/out.json}]
    gate: {type: checks, required_ci: true, repo: x/y, ref: main}
  - id: ai
    role: implementer
    depends_on: [danger]
    prompt: "do work"
    produces: [{name: note, file: artifacts/note.txt}]
inputs:
  - {name: marker, type: string, required: true}
`)
	marker := filepath.Join(t.TempDir(), "side-effect")
	res, err := Run(RunOptions{Root: root, Name: "mocked", Mock: true, Inputs: map[string]string{"marker": marker}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("mock run failed: %+v", res.Steps)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("mock run EXECUTED a run: command")
	}
	if !strings.Contains(res.Steps["danger"].RecordedCmd, "touch") {
		t.Fatalf("command not recorded: %+v", res.Steps["danger"])
	}
	if !strings.Contains(res.Steps["danger"].Note, "gate auto-passed") {
		t.Fatalf("gate not reported auto-passed: %+v", res.Steps["danger"])
	}
	for _, f := range []string{"artifacts/out.json", "artifacts/note.txt"} {
		if _, err := os.Stat(filepath.Join(res.RunDir, f)); err != nil {
			t.Fatalf("mock produce %s missing", f)
		}
	}
}

func TestApprovalPauseResumeAndRefusals(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "gated", hdr("gated")+`steps:
  - id: work
    run: "echo 1 >> $SEED_RUN_DIR/work-count"
  - id: sign-off
    depends_on: [work]
    gate: {type: approval, message: "look first", capture_response: true}
  - id: after
    run: "touch $SEED_RUN_DIR/after"
    depends_on: [sign-off]
`)
	res, err := Run(RunOptions{Root: root, Name: "gated"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "paused" || len(res.NextSteps) == 0 {
		t.Fatalf("approval did not pause: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.RunDir, "after")); err == nil {
		t.Fatal("downstream step ran past a pending approval")
	}
	// Approve and resume: only incomplete steps re-run.
	if err := os.WriteFile(filepath.Join(res.RunDir, "gates", "sign-off.response.json"), []byte(`{"approved": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := Run(RunOptions{Root: root, Name: "gated", Resume: res.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != "succeeded" {
		t.Fatalf("resume: %+v", res2.Steps)
	}
	count, _ := os.ReadFile(filepath.Join(res.RunDir, "work-count"))
	if strings.Count(string(count), "1") != 1 {
		t.Fatalf("completed step re-ran on resume: %q", count)
	}
	// Resuming a succeeded run is refused.
	if _, err := Run(RunOptions{Root: root, Name: "gated", Resume: res.RunID}); err == nil || !strings.Contains(err.Error(), "already succeeded") {
		t.Fatalf("succeeded resume not refused: %v", err)
	}
}

func TestResumeRefusesChangedDefinition(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "drift", hdr("drift")+`steps:
  - id: a
    run: "false"
`)
	res, err := Run(RunOptions{Root: root, Name: "drift"})
	if err != nil || res.Status != "failed" {
		t.Fatalf("setup: %v %+v", err, res)
	}
	writeWF(t, root, "drift", hdr("drift")+`steps:
  - id: a
    run: "true"
`)
	if _, err := Run(RunOptions{Root: root, Name: "drift", Resume: res.RunID}); err == nil || !strings.Contains(err.Error(), "different workflow definition") {
		t.Fatalf("changed-definition resume not refused: %v", err)
	}
}

func TestFailedStepOnlyReexecution(t *testing.T) {
	root := mkroot(t)
	flag := filepath.Join(t.TempDir(), "now-passes")
	writeWF(t, root, "retryable", hdr("retryable")+fmt.Sprintf(`inputs:
  - {name: flag, type: string, required: true}
steps:
  - id: first
    run: "echo 1 >> $SEED_RUN_DIR/first-count"
  - id: flaky
    run: "test -f %s"
    depends_on: [first]
`, flag))
	res, err := Run(RunOptions{Root: root, Name: "retryable", Inputs: map[string]string{"flag": flag}})
	if err != nil || res.Status != "failed" {
		t.Fatalf("setup: %v %+v", err, res)
	}
	if err := os.WriteFile(flag, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := Run(RunOptions{Root: root, Name: "retryable", Inputs: map[string]string{"flag": flag}, Resume: res.RunID})
	if err != nil || res2.Status != "succeeded" {
		t.Fatalf("resume: %v %+v", err, res2)
	}
	count, _ := os.ReadFile(filepath.Join(res.RunDir, "first-count"))
	if strings.Count(string(count), "1") != 1 {
		t.Fatalf("succeeded step re-ran: %q", count)
	}
}

func TestReviewGateRunsRemediation(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "reviewed", hdr("reviewed")+`steps:
  - id: implement
    role: implementer
    prompt: "build it"
    produces: [{name: change, file: artifacts/change.txt}]
  - id: review
    depends_on: [implement]
    gate: {type: review, reviewer_role: reviewer, remediation: remediate, max_revisions: 2}
  - id: remediate
    role: implementer
    run: "echo fixed >> $SEED_RUN_DIR/remediations"
`)
	seq := filepath.Join(t.TempDir(), "seq")
	t.Setenv("FAKE_REVIEW_SEQ", seq)
	res, err := Run(RunOptions{Root: root, Name: "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("review loop: %+v", res.Steps)
	}
	rem, _ := os.ReadFile(filepath.Join(res.RunDir, "remediations"))
	if strings.Count(string(rem), "fixed") != 1 {
		t.Fatalf("remediation did not run exactly once between verdicts: %q", rem)
	}
	if res.Steps["review"].Revisions != 1 {
		t.Fatalf("revisions: %+v", res.Steps["review"])
	}
}

func TestChecksGateHeldByUnresolvedThread(t *testing.T) {
	root := mkroot(t)
	unresolved := true
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{
			{"name": "verify", "status": "completed", "conclusion": "success"}}})
	}))
	defer rest.Close()
	gql := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequest": map[string]any{"reviewThreads": map[string]any{"nodes": []map[string]any{
				{"isResolved": !unresolved}}}}}}})
	}))
	defer gql.Close()
	t.Setenv("GITHUB_TOKEN", "t")
	t.Setenv("SEED_GITHUB_API", rest.URL)
	t.Setenv("SEED_GITHUB_GRAPHQL", gql.URL)
	writeWF(t, root, "landing", hdr("landing")+`steps:
  - id: land
    run: "touch $SEED_RUN_DIR/landed"
    gate: {type: checks, required_ci: true, unresolved_threads: 0, repo: o/r, ref: main, pr: "7"}
`)
	res, err := Run(RunOptions{Root: root, Name: "landing"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || !strings.Contains(res.Steps["land"].Note, "unresolved review thread") {
		t.Fatalf("unresolved thread did not hold the gate: %+v", res.Steps["land"])
	}
	if _, err := os.Stat(filepath.Join(res.RunDir, "landed")); err == nil {
		t.Fatal("land ran despite a closed gate")
	}
	unresolved = false
	res2, err := Run(RunOptions{Root: root, Name: "landing"})
	if err != nil || res2.Status != "succeeded" {
		t.Fatalf("resolved threads should open the gate: %v %+v", err, res2.Steps)
	}
}

func TestStubHonorsConstEnumDefault(t *testing.T) {
	root := mkroot(t)
	writeWF(t, root, "pinned", hdr("pinned")+`steps:
  - id: a
    run: "true"
    output_format:
      type: object
      required: [status, kind, count]
      properties:
        status: {type: string, const: ok}
        kind: {type: string, enum: [alpha, beta]}
        count: {type: integer, default: 7}
    produces: [{name: v, file: artifacts/v.json}]
`)
	res, err := Run(RunOptions{Root: root, Name: "pinned", Mock: true})
	if err != nil || res.Status != "succeeded" {
		t.Fatalf("pinned-constraint stub failed: %v %+v", err, res)
	}
	raw, _ := os.ReadFile(filepath.Join(res.RunDir, "artifacts", "v.json"))
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v["status"] != "ok" || v["kind"] != "alpha" || v["count"] != float64(7) {
		t.Fatalf("stub values: %v", v)
	}
	// An unguessable constraint fails the mock run VISIBLY at the
	// produce check rather than emitting an invalid artifact silently.
	writeWF(t, root, "strict", hdr("strict")+`steps:
  - id: a
    run: "true"
    output_format:
      type: object
      required: [token]
      properties:
        token: {type: string, pattern: "^tok-[0-9]+$"}
    produces: [{name: v, file: artifacts/v.json}]
`)
	res, err = Run(RunOptions{Root: root, Name: "strict", Mock: true})
	if err != nil || res.Status != "failed" || !strings.Contains(res.Steps["a"].Note, "schema") {
		t.Fatalf("unguessable constraint not surfaced: %v %+v", err, res.Steps)
	}
}
