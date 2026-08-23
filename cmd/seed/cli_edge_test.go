package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Commands invoked from a working directory that no longer exists: every
// entry point surfaces the Getwd failure as a usage-class exit.
func TestCLIDeletedCwd(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"task", "ready", "--actor", "x"},
		{"upgrade", "--check"},
		{"skills", "lock"},
		{"template", "upgrade", "--check"},
		{"workflow", "validate", "x"},
		{"mcp", "serve"},
	} {
		if code, _, _ := seedRun(t, args...); code != exitUsage {
			t.Errorf("%v with deleted cwd: exit %d", args, code)
		}
	}
}

// Flag-parse and usage refusals across subcommand routers.
func TestCLIUsageRefusals(t *testing.T) {
	cliFixture(t)
	for _, args := range [][]string{
		{"state", "lint", "--bogusflag"},
		{"maintain", "report", "--bogusflag"},
		{"maintain", "bogus"},
		{"mirror", "record"},
		{"mirror", "record", "os-x"},
		{"mirror", "bogus"},
		{"pr", "classify", "seed/os-1", "--bogusflag"},
		{"receipt", "generate", "os-x", "--bogusflag"},
	} {
		if code, _, _ := seedRun(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want usage", args, code)
		}
	}
}

// mail nudge takes its actor positionally; unknown mail verbs fall back
// to an inbox read.
func TestCLIMailPositionalAndDefault(t *testing.T) {
	cliFixture(t)
	if code, _, _ := seedRun(t, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if code, out, _ := seedRun(t, "mail", "nudge", "lead"); code != 0 {
		t.Fatalf("nudge: %d %s", code, out)
	}
	code, out, _ := seedRun(t, "mail", "peek", "--actor", "lead")
	if code != 0 || !strings.Contains(out, "messages") {
		t.Fatalf("default mail verb: %d %s", code, out)
	}
}

// A config.toml the TOML parser rejects: withService reports the plain
// (non-version-mismatch) service construction failure.
func TestCLIBadConfigService(t *testing.T) {
	root := cliFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".seed", "config.toml"), []byte("[[["), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := seedRun(t, "task", "ready", "--actor", "lead")
	if code == 0 || !strings.Contains(errOut, "seed:") {
		t.Fatalf("bad config tolerated: %d %q", code, errOut)
	}
}

// sync --check red on missing fan-outs, green after apply.
func TestCLISyncCheckDriftAndApply(t *testing.T) {
	cliFixture(t)
	code, _, errOut := seedRun(t, "sync", "--check")
	if code != 1 || !strings.Contains(errOut, "seed sync --check:") {
		t.Fatalf("missing fan-out passed check: %d %q", code, errOut)
	}
	if code, out, _ := seedRun(t, "sync"); code != 0 || !strings.Contains(out, "sync ok") {
		t.Fatalf("apply: %d %q", code, out)
	}
	if code, _, _ := seedRun(t, "sync", "--check"); code != 0 {
		t.Fatal("check red after apply")
	}
}

// validate goes red on a guardrails file that cannot parse.
func TestCLIValidateRed(t *testing.T) {
	root := cliFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".seed", "guardrails.yaml"), []byte("{{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := seedRun(t, "validate")
	if code != 1 || !strings.Contains(errOut, "seed validate:") {
		t.Fatalf("broken guardrails passed validate: %d %q", code, errOut)
	}
}

// backend verify: manifest loads but the lock has no entry for it.
func TestCLIBackendVerifyLockMissing(t *testing.T) {
	cliFixture(t)
	code, _, errOut := seedRun(t, "backend", "verify", "fastcards")
	if code != 1 || !strings.Contains(errOut, "seed backend verify:") {
		t.Fatalf("missing lock entry tolerated: %d %q", code, errOut)
	}
}

// spec lint with an explicit dir whose sibling version file disagrees.
func TestCLISpecLintVersionMismatch(t *testing.T) {
	root := cliFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".seed", "version"), []byte("99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := seedRun(t, "spec", "lint", filepath.Join(root, ".seed", "port-schema"))
	if code != 10 || !strings.Contains(errOut, "seed spec lint:") {
		t.Fatalf("version drift tolerated: %d %q", code, errOut)
	}
}

// receipt generate outside a .seed root falls back to cwd (and then
// fails against a repo with no plan for the task).
func TestCLIReceiptOutsideSeedRoot(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, ".", "init", "-q", "--initial-branch=main", dir)
	t.Chdir(dir)
	code, _, errOut := seedRun(t, "receipt", "generate", "os-deadbeef")
	if code != 1 || !strings.Contains(errOut, "seed receipt generate:") {
		t.Fatalf("rootless receipt: %d %q", code, errOut)
	}
}

// workflow: --all against an unreadable workflows dir, no-root refusals
// for the file-serving subcommands, and a run that ends failed.
func TestCLIWorkflowEdges(t *testing.T) {
	outside := t.TempDir()
	t.Chdir(outside)
	for _, args := range [][]string{
		{"workflow", "validate", "x"},
		{"skills", "lock"},
		{"template", "upgrade", "--check"},
		{"mcp", "serve"},
	} {
		if code, _, errOut := seedRun(t, args...); code != exitUsage || !strings.Contains(errOut, "no .seed") {
			t.Errorf("%v outside root: %d %q", args, code, errOut)
		}
	}

	root := cliFixture(t)
	// .seed/workflows as a file: List errors out under --all.
	if err := os.WriteFile(filepath.Join(root, ".seed", "workflows"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := seedRun(t, "workflow", "validate", "--all"); code != exitUsage {
		t.Fatal("unreadable workflows dir tolerated")
	}
	if err := os.Remove(filepath.Join(root, ".seed", "workflows")); err != nil {
		t.Fatal(err)
	}
	writeF(t, root, ".seed/workflows/doomed.yaml",
		"schema_version: \"1\"\nname: doomed\ndescription: fails\nsteps:\n  - id: boom\n    run: \"exit 1\"\n")
	code, out, _ := seedRun(t, "workflow", "run", "doomed")
	if code != 1 || !strings.Contains(out, "\"status\":\"failed\"") {
		t.Fatalf("failed run: %d %q", code, out)
	}
}
