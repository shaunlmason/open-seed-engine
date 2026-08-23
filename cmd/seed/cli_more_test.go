package main

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

// Deeper CLI paths: a full upgrade against a fake release host, a mock
// workflow run, validate's advisory warnings, backend verification, and
// the no-seed-root refusals every service command shares.

func TestCLIUpgradeSuccessAndCheck(t *testing.T) {
	root := cliFixture(t)
	digest := strings.Repeat("a", 64)
	lock := "version v0.9.0\nrepo shaunlmason/open-seed-engine\n"
	for _, p := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64", "windows_arm64"} {
		lock += "sha256_" + p + " " + digest + "\n"
	}
	writeF(t, root, ".seed/engine.lock", lock)

	newDigest := strings.Repeat("b", 64)
	var checksums strings.Builder
	for _, p := range []struct{ key, ext string }{
		{"darwin_amd64", "tar.gz"}, {"darwin_arm64", "tar.gz"},
		{"linux_amd64", "tar.gz"}, {"linux_arm64", "tar.gz"},
		{"windows_amd64", "zip"}, {"windows_arm64", "zip"},
	} {
		fmt.Fprintf(&checksums, "%s  seed_0.10.0_%s.%s\n", newDigest, p.key, p.ext)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/shaunlmason/open-seed-engine/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/shaunlmason/open-seed-engine/releases/tag/v0.10.0")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/shaunlmason/open-seed-engine/releases/download/v0.10.0/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checksums.String())
	})
	mux.HandleFunc("/shaunlmason/open-seed-engine/releases/download/v0.10.0/protocol.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "1\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("SEED_UPGRADE_BASE_URL", srv.URL)

	code, out, _ := seedRun(t, "upgrade", "--check")
	if code != 0 || !strings.Contains(out, "v0.10.0") {
		t.Fatalf("upgrade --check: %d %s", code, out)
	}
	code, out, _ = seedRun(t, "upgrade")
	if code != 0 || !strings.Contains(out, `"written":true`) {
		t.Fatalf("upgrade: %d %s", code, out)
	}
	after, _ := os.ReadFile(filepath.Join(root, ".seed", "engine.lock"))
	if !strings.Contains(string(after), "version v0.10.0") || !strings.Contains(string(after), newDigest) {
		t.Fatalf("pin not moved:\n%s", after)
	}
}

func TestCLIWorkflowValidateAndMockRun(t *testing.T) {
	root := cliFixture(t)
	writeF(t, root, ".seed/workflows/tiny.yaml", `schema_version: "1"
name: tiny
description: cli coverage workflow
steps:
  - id: a
    run: "echo hello"
    produces: [{name: x, file: artifacts/x.txt}]
  - id: b
    run: "cat {{output.x.path}}"
    depends_on: [a]
`)
	t.Setenv("SEED_RUNS_DIR", t.TempDir())
	code, out, errS := seedRun(t, "workflow", "validate", "--all")
	if code != 0 {
		t.Fatalf("workflow validate: %d %s %s", code, out, errS)
	}
	code, out, errS = seedRun(t, "workflow", "validate", filepath.Join(root, ".seed", "workflows", "tiny.yaml"))
	if code != 0 {
		t.Fatalf("workflow validate one: %d %s %s", code, out, errS)
	}
	code, out, errS = seedRun(t, "workflow", "run", "tiny", "--mock")
	if code != 0 {
		t.Fatalf("workflow run --mock: %d %s %s", code, out, errS)
	}
	// A validation finding exits 3.
	writeF(t, root, ".seed/workflows/broken.yaml", `schema_version: "1"
name: broken
description: broken
steps:
  - id: a
`)
	if code, _, _ = seedRun(t, "workflow", "validate", "--all"); code != 3 {
		t.Fatalf("broken workflow validate exit %d, want 3", code)
	}
}

func TestCLIValidateWarningsPath(t *testing.T) {
	root := cliFixture(t)
	// A mission activates goal-ancestry; an unrooted open card warns.
	writeF(t, root, ".seed/teams/core.yaml", "name: core\nmission: ship\nlead: alice\nscope: [\"**\"]\npriority: 1\ntier: L2\nreview: codeowners\n")
	seedRun(t, "init")
	seedRun(t, "task", "create", "--title", "unrooted", "--actor", "a")
	code, out, errS := seedRun(t, "validate")
	if code != 0 || !strings.Contains(errS, "warning") {
		t.Fatalf("validate warnings: %d %s %s", code, out, errS)
	}
}

func TestCLIBackendVerifyBuiltinManifest(t *testing.T) {
	root := cliFixture(t)
	writeF(t, root, ".seed/backends/mini/backend.toml", `name = "mini"
version = "0.0.1"
schema_version = "1.0"
entry = "builtin"

[capabilities]
required = true
atomic_claim = "native"
offline = "native"
`)
	writeF(t, root, ".seed/backends.lock.json", `{"mini": {"source": "builtin", "engine": ">=0.1.0"}}`)
	code, out, errS := seedRun(t, "backend", "verify", "mini")
	if code != 0 || !strings.Contains(out, "backend mini ok") {
		t.Fatalf("backend verify: %d %s %s", code, out, errS)
	}
}

func TestCLIStateImportFromStdin(t *testing.T) {
	cliFixture(t)
	seedRun(t, "init")
	seedRun(t, "task", "create", "--title", "X", "--actor", "a")
	_, exportOut, _ := seedRun(t, "state", "export")
	var exp struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal([]byte(exportOut), &exp); err != nil {
		t.Fatal(err)
	}

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
		w.Write(exp.Document)
		w.Close()
	}()
	code, out, errS := seedRun(t, "state", "import", "--actor", "lead", "-")
	if code != 0 {
		t.Fatalf("stdin import: %d %s %s", code, out, errS)
	}
}

func TestCLIRefusalsOutsideSeedRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"task", "list"},
		{"validate"},
		{"sync"},
		{"backend", "verify", "x"},
		{"mcp", "serve"},
		{"upgrade"},
		{"init"},
		{"maintain", "report"},
	} {
		if code, _, _ := seedRun(t, args...); code == 0 {
			t.Errorf("%v succeeded outside a seed root", args)
		}
	}
}

func TestCLISpecLintExplicitDirAndMailFlagErrors(t *testing.T) {
	root := cliFixture(t)
	seedRun(t, "init")
	if code, _, _ := seedRun(t, "spec", "lint", filepath.Join(root, ".seed", "port-schema")); code != 0 {
		t.Fatal("spec lint with explicit dir failed")
	}
	if code, _, _ := seedRun(t, "mail", "send", "--definitely-not-a-flag"); code != exitUsage {
		t.Fatal("bad mail flag not usage")
	}
	if code, _, _ := seedRun(t, "handoff", "generate", "os-x", "--bad-flag"); code != exitUsage {
		t.Fatal("bad handoff flag not usage")
	}
	if code, _, _ := seedRun(t, "task", "create", "--bad-flag"); code != exitUsage {
		t.Fatal("bad task flag not usage")
	}
	if code, _, _ := seedRun(t, "maintain", "report", "--stalled-after", "1h"); code != 0 {
		t.Fatal("maintain report with duration failed")
	}
}

func TestCLIVersionMismatchExit10(t *testing.T) {
	root := cliFixture(t)
	writeF(t, root, ".seed/version", "99\n")
	code, out, _ := seedRun(t, "task", "list")
	if code != 10 || !strings.Contains(out, "version_mismatch") {
		t.Fatalf("version mismatch: %d %s", code, out)
	}
}

func TestCLIExternalBackendDispatch(t *testing.T) {
	root := cliFixture(t)
	// A plugin backend: entry is an executable, envelope-valid on stdout.
	writeF(t, root, ".seed/config.toml",
		"[coordination]\nbackend = \"plug\"\n[operators]\nactors = [\"lead\"]\n")
	writeF(t, root, ".seed/backends/plug/backend.toml", `name = "plug"
version = "0.0.1"
schema_version = "1.0"
entry = "run.sh"

[capabilities]
required = true
atomic_claim = "native"
offline = "native"
`)
	writeF(t, root, ".seed/backends/plug/run.sh", "#!/bin/sh\nprintf '{\"ok\":true,\"schema_version\":\"1.0\",\"verb\":\"%s\"}\\n' \"$1\"\n")
	if err := os.Chmod(filepath.Join(root, ".seed", "backends", "plug", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeF(t, root, ".seed/backends.lock.json", `{"plug": {"source": "in-template", "engine": ">=0.1.0"}}`)
	code, out, errS := seedRun(t, "task", "list")
	if code != 0 || !strings.Contains(out, `"verb":"list"`) {
		t.Fatalf("plugin dispatch: %d %s %s", code, out, errS)
	}
	// A plugin emitting a schema-invalid envelope is refused (exit 10).
	writeF(t, root, ".seed/backends/plug/run.sh", "#!/bin/sh\necho not-json\n")
	os.Chmod(filepath.Join(root, ".seed", "backends", "plug", "run.sh"), 0o755)
	if code, _, _ = seedRun(t, "task", "list"); code != 10 {
		t.Fatalf("invalid plugin envelope exit %d, want 10", code)
	}
	// A configured-but-missing backend manifest is unavailable (exit 5).
	writeF(t, root, ".seed/config.toml", "[coordination]\nbackend = \"ghost\"\n")
	if code, _, _ = seedRun(t, "task", "list"); code != 5 {
		t.Fatalf("ghost backend exit %d, want 5", code)
	}
}

func TestCLIWorkflowUsageAndInputParsing(t *testing.T) {
	root := cliFixture(t)
	writeF(t, root, ".seed/workflows/in.yaml", `schema_version: "1"
name: in
description: input echo
inputs:
  - {name: who, type: string, required: true}
  - {name: what, type: string, required: true}
steps:
  - id: a
    run: "echo {{inputs.who}} {{inputs.what}}"
`)
	t.Setenv("SEED_RUNS_DIR", t.TempDir())
	code, _, _ := seedRun(t, "workflow", "run", "in", "--mock", "--input", "who=us", "--input", "what=cov")
	if code != 0 {
		t.Fatal("mock run with inputs failed")
	}
	if code, _, _ = seedRun(t, "workflow", "validate"); code != exitUsage {
		t.Fatal("argless validate not usage")
	}
	if code, _, _ = seedRun(t, "workflow", "run", "ghost", "--mock"); code == 0 {
		t.Fatal("unknown workflow ran")
	}
	if code, _, _ = seedRun(t, "workflow", "run", "in", "--input", "malformed"); code != exitUsage {
		t.Fatal("malformed --input not usage")
	}
	if code, _, _ = seedRun(t, "workflow", "bogus"); code != exitUsage {
		t.Fatal("unknown workflow subcommand not usage")
	}
	if code, _, _ = seedRun(t, "workflow", "validate", "--bad-flag"); code != exitUsage {
		t.Fatal("bad validate flag not usage")
	}
}

func TestCLISkillsAndTemplateEdges(t *testing.T) {
	root := cliFixture(t)
	writeF(t, root, "seed.yaml", "schema_version: \"1\"\nskills:\n  - {name: ghost, repo: /nonexistent/repo, ref: v1}\n")
	if code, out, _ := seedRun(t, "skills", "lock"); code != 1 || !strings.Contains(out, "skills_refused") {
		t.Fatalf("unreachable skill lock: %d %s", code, out)
	}
	writeF(t, root, "seed.yaml", "schema_version: \"1\"\nskills: [\n")
	if code, _, _ := seedRun(t, "skills", "install"); code != 1 {
		t.Fatal("bad manifest install passed")
	}
	if code, _, _ := seedRun(t, "skills", "bogus"); code != exitUsage {
		t.Fatal("unknown skills subcommand not usage")
	}

	// template upgrade --check against a fake release host: the success
	// JSON path with no git involved.
	writeF(t, root, ".seed/template.lock", "repo shaunlmason/open-seed-engine\nversion v0.1.0\n")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "template lock")
	mux := http.NewServeMux()
	mux.HandleFunc("/shaunlmason/open-seed-engine/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/shaunlmason/open-seed-engine/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("SEED_UPGRADE_BASE_URL", srv.URL)
	code, out, _ := seedRun(t, "template", "upgrade", "--check")
	if code != 0 || !strings.Contains(out, "v0.2.0") {
		t.Fatalf("template check: %d %s", code, out)
	}
	if code, _, _ := seedRun(t, "template", "upgrade", "--bad-flag"); code != exitUsage {
		t.Fatal("bad template flag not usage")
	}
}

func TestCLIStateResumeEmptyActorAndReceiptWrite(t *testing.T) {
	root := cliFixture(t)
	seedRun(t, "init")
	if code, _, _ := seedRun(t, "state", "resume", "--actor", ""); code != exitUsage {
		t.Fatal("empty-actor resume not usage")
	}
	if code, _, _ := seedRun(t, "pr", "classify", "seed/os-1234abcd-plan", "--files", "plans/os-1234abcd.md,src/x.go"); code != 1 {
		t.Fatal("impure plan PR passed")
	}
	// receipt verify --write regenerates and persists.
	good := "# Plan: r (os-cafe0001)\n\n## Steps\n\n1. Add src/y.\n\n## File Scope\n\n- src/\n\n## Acceptance Criteria\n\n- ok.\n\n## Validation Commands\n\n- `true`\n"
	writeF(t, root, "plans/os-cafe0001.md", good)
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "plan")
	gitIn(t, root, "checkout", "-qb", "seed/os-cafe0001")
	writeF(t, root, "src/y", "v\n")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "impl")
	if code, _, errS := seedRun(t, "receipt", "generate", "os-cafe0001", "--base", "main", "--write"); code != 0 {
		t.Fatalf("generate: %d %s", code, errS)
	}
	gitIn(t, root, "add", "receipts")
	gitIn(t, root, "commit", "-qm", "receipt")
	if code, _, errS := seedRun(t, "receipt", "verify", "os-cafe0001", "--base", "main", "--branch", "seed/os-cafe0001", "--write"); code != 0 {
		t.Fatalf("verify --write: %d %s", code, errS)
	}
}
