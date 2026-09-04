package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// CLI-level tests: every subcommand runs through run() against a scratch
// fastcards repo, exactly as a user invokes it: argument parsing, service
// construction, envelope emission, and exit codes all covered at the seam
// the template scripts actually call.

func writeF(t *testing.T, root, p, c string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// cliFixture builds a minimal instantiation on the fastcards backend and
// chdirs into it.
func cliFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	gitIn(t, ".", "init", "-q", "--initial-branch=main", root)
	_, thisFile, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(thisFile), "..", "..", "internal", "spec", "testdata", "seed")
	entries, err := os.ReadDir(filepath.Join(src, "port-schema"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, "port-schema", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		writeF(t, root, filepath.Join(".seed", "port-schema", e.Name()), string(b))
	}
	if b, err := os.ReadFile(filepath.Join(src, "version")); err == nil {
		writeF(t, root, ".seed/version", string(b))
	} else {
		writeF(t, root, ".seed/version", "1\n")
	}
	writeF(t, root, ".seed/config.toml",
		"[coordination]\nbackend = \"fastcards\"\n[operators]\nactors = [\"lead\"]\n[claim]\ndefault_lease = \"60m\"\n")
	writeF(t, root, ".seed/backends/fastcards/backend.toml", `name = "fastcards"
version = "0.1.0"
schema_version = "1.0"
entry = "builtin"

[capabilities]
required = true
optional = ["lease-renew"]
atomic_claim = "native"
offline = "native"
budget = "none"
state_portability = "machine"
`)
	// validate surface
	writeF(t, root, ".seed/guardrails.yaml", `autonomy:
  default_tier: L1
  max_tier: L2
protected_paths:
  - .seed/**
auto_merge_allowlist: []
`)
	writeF(t, root, ".seed/teams/core.yaml", "name: core\nlead: alice\nscope: [\"**\"]\npriority: 1\ntier: L2\n")
	writeF(t, root, ".seed/agents/reviewer.md", "---\nname: reviewer\ndescription: Review a task PR.\nrole: reviewer\n---\n\n## Task\n\nReview.\n")
	if err := os.MkdirAll(filepath.Join(root, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return root
}

// seedRun invokes run() with real files for stdout/stderr and returns
// exit code + captured streams.
func seedRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	outF, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	code := run(args, outF, errF)
	outB, _ := os.ReadFile(outF.Name())
	errB, _ := os.ReadFile(errF.Name())
	outF.Close()
	errF.Close()
	return code, string(outB), string(errB)
}

func mustJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("not a JSON envelope: %q (%v)", out, err)
	}
	return m
}

func TestCLITaskLifecycle(t *testing.T) {
	cliFixture(t)
	if code, _, _ := seedRun(t, "init"); code != 0 {
		t.Fatalf("init exit %d", code)
	}
	code, out, _ := seedRun(t, "task", "create", "--title", "A card", "--body", "work",
		"--priority", "P1", "--label", "x", "--actor", "a")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}
	id := mustJSON(t, out)["task"].(string)

	if code, _, _ = seedRun(t, "task", "create", "--actor", "a"); code != exitUsage {
		t.Fatalf("titleless create exit %d, want usage", code)
	}
	if code, _, _ = seedRun(t, "task", "get"); code != exitUsage {
		t.Fatalf("get without id exit %d, want usage", code)
	}
	if code, _, _ = seedRun(t, "task", "frobnicate", id); code != exitUsage {
		t.Fatalf("unknown verb exit %d, want usage", code)
	}
	if code, _, _ = seedRun(t, "task", "get", "os-nope"); code != 4 {
		t.Fatalf("missing card exit %d, want 4", code)
	}

	if code, _, _ = seedRun(t, "task", "promote", id, "--actor", "lead"); code != 0 {
		t.Fatalf("promote exit %d", code)
	}
	code, out, _ = seedRun(t, "task", "ready", "--actor", "b")
	if code != 0 || !strings.Contains(out, id) {
		t.Fatalf("ready: %d %s", code, out)
	}
	code, out, _ = seedRun(t, "task", "list", "--state", "ready")
	if code != 0 || !strings.Contains(out, id) {
		t.Fatalf("list: %d %s", code, out)
	}

	code, out, _ = seedRun(t, "task", "claim", id, "--actor", "b", "--lease", "30m")
	if code != 0 {
		t.Fatalf("claim exit %d: %s", code, out)
	}
	tok := mustJSON(t, out)["claim_token"].(string)
	if code, _, _ = seedRun(t, "task", "lease-renew", id, "--actor", "b", "--token", tok); code != 0 {
		t.Fatal("lease-renew failed")
	}
	code, out, _ = seedRun(t, "task", "comment", id, "--actor", "b", "--body", "hi", "--token", tok)
	if code != 0 || mustJSON(t, out)["comment_id"] == nil {
		t.Fatalf("comment: %d %s", code, out)
	}
	code, out, _ = seedRun(t, "task", "attach-evidence", id, "--actor", "b", "--kind", "commit", "--ref", "abc", "--token", tok)
	if code != 0 || mustJSON(t, out)["evidence_id"] == nil {
		t.Fatalf("attach-evidence: %d %s", code, out)
	}
	// Fence holds at the CLI seam too.
	if code, _, _ = seedRun(t, "task", "comment", id, "--actor", "b", "--body", "x", "--token", "bogus"); code != 6 {
		t.Fatal("bogus token not fenced")
	}
	if code, _, _ = seedRun(t, "task", "release", id, "--actor", "b", "--token", tok); code != 0 {
		t.Fatal("release failed")
	}

	code, out, _ = seedRun(t, "task", "claim", id, "--actor", "b")
	tok = mustJSON(t, out)["claim_token"].(string)
	if code, _, _ = seedRun(t, "task", "transition", id, "--to", "review", "--actor", "b", "--token", tok); code != 0 {
		t.Fatal("review transition failed")
	}
	if code, _, _ = seedRun(t, "task", "reject", id, "--actor", "lead", "--resolution", "again"); code != 0 {
		t.Fatal("reject failed")
	}
	code, out, _ = seedRun(t, "task", "claim", id, "--actor", "c")
	tok = mustJSON(t, out)["claim_token"].(string)
	seedRun(t, "task", "transition", id, "--to", "review", "--actor", "c", "--token", tok)
	code, out, _ = seedRun(t, "task", "close", id, "--actor", "lead", "--resolution", "done")
	if code != 0 || mustJSON(t, out)["state"] != "done" {
		t.Fatalf("close: %d %s", code, out)
	}

	// Parking + plan-unblock + cancel/reinstate through the CLI.
	code, out, _ = seedRun(t, "task", "create", "--title", "B", "--actor", "a")
	id2 := mustJSON(t, out)["task"].(string)
	seedRun(t, "task", "promote", id2, "--actor", "lead")
	if code, _, _ = seedRun(t, "task", "block", id2, "--actor", "lead", "--blocked-on", "plan:7"); code != 0 {
		t.Fatal("block failed")
	}
	code, out, _ = seedRun(t, "task", "plan-unblock", id2, "--pr", "7", "--actor", "lead")
	if code != 0 || mustJSON(t, out)["state"] != "ready" {
		t.Fatalf("plan-unblock: %d %s", code, out)
	}
	if code, _, _ = seedRun(t, "task", "cancel", id2, "--actor", "lead", "--resolution", "nope"); code != 0 {
		t.Fatal("cancel failed")
	}
	if code, _, _ = seedRun(t, "task", "reinstate", id2, "--actor", "lead"); code != 0 {
		t.Fatal("reinstate failed")
	}
	if code, _, _ = seedRun(t, "task"); code != exitUsage {
		t.Fatal("bare task not usage")
	}
}

func TestCLIMailAndHandoff(t *testing.T) {
	cliFixture(t)
	seedRun(t, "init")
	if code, _, _ := seedRun(t, "mail", "send", "--actor", "a", "--to", "b", "--type", "info", "--text", "hello"); code != 0 {
		t.Fatal("mail send failed")
	}
	code, out, _ := seedRun(t, "mail", "read", "--actor", "b", "--unread")
	if code != 0 || !strings.Contains(out, "hello") {
		t.Fatalf("mail read: %d %s", code, out)
	}
	var env struct {
		Messages []struct{ ID string }
	}
	json.Unmarshal([]byte(out), &env)
	if len(env.Messages) != 1 {
		t.Fatalf("messages: %s", out)
	}
	if code, _, _ = seedRun(t, "mail", "ack", "--actor", "b", "--id", env.Messages[0].ID); code != 0 {
		t.Fatal("mail ack failed")
	}
	if code, _, _ = seedRun(t, "mail", "nudge", "--actor", "b"); code != 0 {
		t.Fatal("mail nudge failed")
	}
	if code, _, _ = seedRun(t, "mail", "prune"); code != 0 {
		t.Fatal("mail prune failed")
	}
	if code, _, _ = seedRun(t, "mail"); code != exitUsage {
		t.Fatal("bare mail not usage")
	}

	code, out, _ = seedRun(t, "task", "create", "--title", "H", "--actor", "a")
	id := mustJSON(t, out)["task"].(string)
	code, out, _ = seedRun(t, "handoff", "generate", id)
	if code != 0 || !strings.Contains(out, "packet") {
		t.Fatalf("handoff generate: %d %s", code, out)
	}
	if code, _, _ = seedRun(t, "handoff", "generate", id, "--write", "--actor", "a"); code != 0 {
		t.Fatal("handoff --write failed")
	}
	if code, _, _ = seedRun(t, "handoff"); code != exitUsage {
		t.Fatal("bare handoff not usage")
	}
}

func TestCLIStateMaintainMirror(t *testing.T) {
	cliFixture(t)
	seedRun(t, "init")
	seedRun(t, "task", "create", "--title", "S", "--actor", "a")

	if code, out, _ := seedRun(t, "state", "lint", "--actor", "lead"); code != 0 {
		t.Fatalf("state lint: %d %s", code, out)
	}
	code, exportOut, _ := seedRun(t, "state", "export")
	if code != 0 || !strings.Contains(exportOut, "cards") {
		t.Fatalf("state export: %d", code)
	}
	// resume with no HALT present still exercises the operator path.
	seedRun(t, "state", "resume", "--actor", "lead")
	// anchor declares itself not applicable on the machine-local store.
	seedRun(t, "state", "anchor")
	if code, _, _ = seedRun(t, "state", "bogus"); code != exitUsage {
		t.Fatal("state bogus not usage")
	}
	if code, _, _ = seedRun(t, "state"); code != exitUsage {
		t.Fatal("bare state not usage")
	}

	if code, _, _ = seedRun(t, "maintain", "reap", "--actor", "lead"); code != 0 {
		t.Fatal("maintain reap failed")
	}
	if code, out, _ := seedRun(t, "maintain", "report"); code != 0 || !strings.Contains(out, "states") {
		t.Fatalf("maintain report: %d %s", code, out)
	}
	if code, _, _ = seedRun(t, "maintain", "reap"); code != exitUsage {
		t.Fatal("actorless reap not usage")
	}
	if code, _, _ = seedRun(t, "maintain"); code != exitUsage {
		t.Fatal("bare maintain not usage")
	}

	seedRun(t, "mirror", "plan")
	seedRun(t, "mirror", "record", "os-nope", "--issue", "1", "--state", "ready", "--actor", "lead")
	if code, _, _ = seedRun(t, "mirror", "record", "os-nope"); code != exitUsage {
		t.Fatal("flagless mirror record not usage")
	}
	if code, _, _ = seedRun(t, "mirror"); code != exitUsage {
		t.Fatal("bare mirror not usage")
	}

	// Import round-trip into a fresh, un-initialized store: the export
	// DOCUMENT rides the envelope's "document" field.
	var exp struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal([]byte(exportOut), &exp); err != nil {
		t.Fatalf("export envelope: %v", err)
	}
	exportFile := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(exportFile, exp.Document, 0o644); err != nil {
		t.Fatal(err)
	}
	cliFixture(t)      // fresh repo, chdir'd
	seedRun(t, "init") // empty store: import's precondition
	if code, out, _ := seedRun(t, "state", "import", "--actor", "lead", exportFile); code != 0 {
		t.Fatalf("state import: %d %s", code, out)
	}
	if code, _, _ := seedRun(t, "state", "import", filepath.Join(t.TempDir(), "missing.json")); code != exitUsage {
		t.Fatal("missing import file not usage")
	}
}

func TestCLISpecPlanPRValidateSync(t *testing.T) {
	root := cliFixture(t)
	if code, out, _ := seedRun(t, "spec", "lint"); code != 0 {
		t.Fatalf("spec lint: %d %s", code, out)
	}
	if code, _, _ := seedRun(t, "spec", "lint", filepath.Join(t.TempDir(), "nope")); code == 0 {
		t.Fatal("spec lint on a missing dir passed")
	}
	if code, _, _ := seedRun(t, "spec"); code != exitUsage {
		t.Fatal("bare spec not usage")
	}

	good := "# Plan: x (os-1234abcd)\n\n## Steps\n\n1. Do it.\n\n## File Scope\n\n- src/\n\n## Acceptance Criteria\n\n- Done.\n\n## Validation Commands\n\n- `true`\n"
	writeF(t, root, "plans/os-1234abcd.md", good)
	if code, out, _ := seedRun(t, "plan", "lint", "plans/os-1234abcd.md"); code != 0 {
		t.Fatalf("plan lint: %d %s", code, out)
	}
	badPlan := filepath.Join(t.TempDir(), "bad.md")
	if err := os.WriteFile(badPlan, []byte("# not a plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := seedRun(t, "plan", "lint", badPlan); code != 1 {
		t.Fatal("bad plan passed lint")
	}
	if code, _, _ := seedRun(t, "plan", "lint", "plans/none.md"); code != 1 {
		t.Fatal("missing plan file not an error")
	}
	if code, _, _ := seedRun(t, "plan"); code != exitUsage {
		t.Fatal("bare plan not usage")
	}

	code, out, _ := seedRun(t, "pr", "classify", "seed/os-1234abcd", "--files", "src/a.go")
	if code != 0 || !strings.Contains(out, "class=task") || !strings.Contains(out, "purity ok") {
		t.Fatalf("pr classify: %d %s", code, out)
	}
	if code, _, _ = seedRun(t, "pr", "classify", "seed/os-1234abcd", "--files", "plans/os-1234abcd.md"); code != 1 {
		t.Fatal("impure task PR passed")
	}
	if code, _, _ = seedRun(t, "pr"); code != exitUsage {
		t.Fatal("bare pr not usage")
	}

	if code, out, errS := seedRun(t, "validate"); code != 0 {
		t.Fatalf("validate: %d %s %s", code, out, errS)
	}

	// sync: source tree → fan-outs, then --check is clean.
	writeF(t, root, "skills/greet/SKILL.md", "# greet\n\nHi.\n")
	writeF(t, root, "rules/00-a.md", "# A\n\n- rule one\n")
	writeF(t, root, "AGENTS.md", "# AGENTS.md\n\n<!-- seed:rules:begin x -->\nstale\n<!-- seed:rules:end -->\n")
	if code, _, _ := seedRun(t, "sync", "--check"); code != 1 {
		t.Fatal("pre-sync check should fail")
	}
	if code, out, _ := seedRun(t, "sync"); code != 0 || !strings.Contains(out, "written") {
		t.Fatalf("sync: %d %s", code, out)
	}
	if code, _, _ := seedRun(t, "sync", "--check"); code != 0 {
		t.Fatal("post-sync check dirty")
	}

	if code, _, _ := seedRun(t, "backend", "verify", "ghost"); code != 1 {
		t.Fatal("missing backend verified")
	}
	if code, _, _ := seedRun(t, "backend"); code != exitUsage {
		t.Fatal("bare backend not usage")
	}
	if code, out, _ := seedRun(t, "init-github"); code != 0 || !strings.Contains(out, "protection") {
		t.Fatal("init-github")
	}
}

func TestCLISkillsAndWorkflow(t *testing.T) {
	root := cliFixture(t)
	writeF(t, root, "seed.yaml", "schema_version: \"1\"\nskills: []\n")
	if code, _, _ := seedRun(t, "skills", "lock"); code != 0 {
		t.Fatal("skills lock failed")
	}
	if code, _, _ := seedRun(t, "skills", "install", "--frozen"); code != 0 {
		t.Fatal("skills install --frozen failed")
	}
	if code, _, _ := seedRun(t, "skills"); code != exitUsage {
		t.Fatal("bare skills not usage")
	}

	seedRun(t, "workflow", "validate", "--all")
	if code, _, _ := seedRun(t, "workflow"); code != exitUsage {
		t.Fatal("bare workflow not usage")
	}
	if code, _, _ := seedRun(t, "workflow", "run"); code != exitUsage {
		t.Fatal("nameless workflow run not usage")
	}
}

func TestCLIUpgradeAndTemplate(t *testing.T) {
	cliFixture(t)
	// No engine.lock: a structured refusal, not a crash.
	code, out, _ := seedRun(t, "upgrade", "--check")
	if code == 0 {
		t.Fatalf("upgrade without engine.lock succeeded: %s", out)
	}
	mustJSON(t, out)
	if code, _, _ := seedRun(t, "upgrade", "--definitely-not-a-flag"); code != exitUsage {
		t.Fatal("bad upgrade flag not usage")
	}
	// No template.lock: refusal path.
	if code, _, _ := seedRun(t, "template", "upgrade", "--check"); code == 0 {
		t.Fatal("template upgrade without lock succeeded")
	}
	if code, _, _ := seedRun(t, "template"); code != exitUsage {
		t.Fatal("bare template not usage")
	}
}

func TestCLIMCPServe(t *testing.T) {
	cliFixture(t)
	seedRun(t, "init")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()
	go func() {
		w.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}` + "\n")
		w.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
		w.Close()
	}()
	code, out, _ := seedRun(t, "mcp", "serve")
	if code != 0 || !strings.Contains(out, "protocolVersion") || !strings.Contains(out, "task_create") {
		t.Fatalf("mcp serve: %d %s", code, out)
	}
	if code, _, _ := seedRun(t, "mcp"); code != exitUsage {
		t.Fatal("bare mcp not usage")
	}
}

func TestCLIReceiptFlow(t *testing.T) {
	root := cliFixture(t)
	good := "# Plan: r (os-feedbeef)\n\n## Steps\n\n1. Add src/x.\n\n## File Scope\n\n- src/\n\n## Acceptance Criteria\n\n- src/x exists.\n\n## Validation Commands\n\n- `test -f src/x`\n"
	writeF(t, root, "plans/os-feedbeef.md", good)
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "base with plan")
	gitIn(t, root, "checkout", "-qb", "seed/os-feedbeef")
	writeF(t, root, "src/x", "content\n")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "implement")

	code, out, errS := seedRun(t, "receipt", "generate", "os-feedbeef", "--base", "main", "--run", "--write")
	if code != 0 {
		t.Fatalf("receipt generate: %d %s %s", code, out, errS)
	}
	gitIn(t, root, "add", "receipts")
	gitIn(t, root, "commit", "-qm", "receipt")

	code, out, errS = seedRun(t, "receipt", "verify", "os-feedbeef", "--base", "main", "--branch", "seed/os-feedbeef", "--run")
	if code != 0 || !strings.Contains(out, "verify ok") {
		t.Fatalf("receipt verify: %d %s %s", code, out, errS)
	}
	if code, _, _ = seedRun(t, "receipt", "verify", "os-feedbeef", "--base", "main"); code != exitUsage {
		t.Fatal("branchless verify not usage")
	}
	if code, _, _ = seedRun(t, "receipt", "generate", "os-nope", "--base", "main"); code != 1 {
		t.Fatal("planless generate not refused")
	}
	// --emit writes the attestation CI uploads: the claim plus the snapshot
	// it verified, which the committed file deliberately no longer carries.
	att := filepath.Join(t.TempDir(), "attestation.json")
	code, out, errS = seedRun(t, "receipt", "verify", "os-feedbeef", "--base", "main",
		"--branch", "seed/os-feedbeef", "--run", "--emit", att, "--by", "ci:run/7")
	if code != 0 {
		t.Fatalf("receipt verify --emit: %d %s %s", code, out, errS)
	}
	b, err := os.ReadFile(att)
	if err != nil {
		t.Fatalf("attestation not emitted: %v", err)
	}
	for _, want := range []string{"merge_base", "diff_sha256", "changed_files", "ci:run/7"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("attestation missing %q: %s", want, b)
		}
	}
	if committed, _ := os.ReadFile(filepath.Join(root, "receipts", "os-feedbeef.json")); strings.Contains(string(committed), "merge_base") {
		t.Fatalf("the committed receipt carries the snapshot: %s", committed)
	}

	// migrate is addressed by task or --all, and is a no-op at the current
	// schema.
	if code, out, errS = seedRun(t, "receipt", "migrate", "--all"); code != 0 || !strings.Contains(out, "0 of 1") {
		t.Fatalf("receipt migrate --all: %d %s %s", code, out, errS)
	}
	if code, _, _ = seedRun(t, "receipt", "migrate", "os-feedbeef", "--all"); code != exitUsage {
		t.Fatal("receipt migrate with both a task and --all not usage")
	}
	if code, _, _ = seedRun(t, "receipt", "migrate"); code != exitUsage {
		t.Fatal("targetless receipt migrate not usage")
	}

	// A task id names one file under receipts/, and an attestation never
	// belongs in that directory: both refusals happen before any git work.
	for _, bad := range []string{"../../settings", "a/b", ".."} {
		if code, _, errS = seedRun(t, "receipt", "migrate", bad); code != exitUsage {
			t.Fatalf("receipt migrate %q not usage: %d %s", bad, code, errS)
		}
		if code, _, _ = seedRun(t, "receipt", "generate", bad, "--base", "main", "--write"); code != exitUsage {
			t.Fatalf("receipt generate %q not usage", bad)
		}
	}
	if code, _, errS = seedRun(t, "receipt", "verify", "os-feedbeef", "--base", "main",
		"--branch", "seed/os-feedbeef", "--emit", filepath.Join(root, "receipts", "os-feedbeef.json")); code != exitUsage {
		t.Fatalf("emitting the attestation into receipts/ not usage: %d %s", code, errS)
	}

	if code, _, _ = seedRun(t, "receipt", "bogus", "os-feedbeef"); code != exitUsage {
		t.Fatal("receipt bogus not usage")
	}
	if code, _, _ = seedRun(t, "receipt"); code != exitUsage {
		t.Fatal("bare receipt not usage")
	}
}
