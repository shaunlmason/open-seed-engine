package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/config"
	"github.com/shaunlmason/open-seed-engine/internal/workflow"
)

// runWorkflow: `seed workflow validate|run` (plan os-52b9aed0). Not a port
// verb — workflows are the intra-run DAG; any task-state mutation a step
// makes goes through `scripts/seed task <verb>` like every other caller.
func runWorkflow(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "seed: usage: seed workflow validate [<file>|--all] | seed workflow run <name> [--input k=v]... [--mock] [--resume <run-id>]")
		return exitUsage
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "seed:", err)
		return exitUsage
	}
	root, found := config.FindRoot(cwd)
	if !found {
		fmt.Fprintln(stderr, "seed: no .seed directory found (run from an open-seed repo)")
		return exitUsage
	}
	switch args[0] {
	case "validate":
		fs := flag.NewFlagSet("workflow validate", flag.ContinueOnError)
		fs.SetOutput(stderr)
		all := fs.Bool("all", false, "validate every checked-in workflow")
		withHarnesses := fs.Bool("with-harnesses", false, "also check harness adapters exist")
		if fs.Parse(args[1:]) != nil {
			return exitUsage
		}
		var files []string
		if *all {
			files, err = workflow.List(root)
			if err != nil {
				fmt.Fprintln(stderr, "seed:", err)
				return exitUsage
			}
		} else if fs.NArg() == 1 {
			files = []string{fs.Arg(0)}
		} else {
			fmt.Fprintln(stderr, "seed: workflow validate needs a file or --all")
			return exitUsage
		}
		type fileFindings struct {
			File     string             `json:"file"`
			Findings []workflow.Finding `json:"findings"`
		}
		var report []fileFindings
		bad := false
		for _, f := range files {
			findings := workflow.Validate(root, f, *withHarnesses)
			rel, _ := filepath.Rel(root, f)
			report = append(report, fileFindings{File: rel, Findings: findings})
			if len(findings) > 0 {
				bad = true
			}
		}
		b, _ := json.Marshal(map[string]any{"ok": !bad, "schema_version": "1.0", "verb": "workflow-validate",
			"workflows": report})
		fmt.Fprintln(stdout, string(b))
		if bad {
			return 3
		}
		return 0
	case "run":
		fs := flag.NewFlagSet("workflow run", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var inputs inputFlags
		fs.Var(&inputs, "input", "workflow input k=v (repeatable)")
		mock := fs.Bool("mock", false, "zero-credential, zero-side-effect run via the mock harness")
		resume := fs.String("resume", "", "resume a paused or failed run by id")
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			fmt.Fprintln(stderr, "seed: workflow run needs a workflow name")
			return exitUsage
		}
		name := args[1]
		if fs.Parse(args[2:]) != nil {
			return exitUsage
		}
		res, rerr := workflow.Run(workflow.RunOptions{Root: root, Name: name, Inputs: inputs.m, Mock: *mock, Resume: *resume})
		if rerr != nil {
			b, _ := json.Marshal(map[string]any{"ok": false, "schema_version": "1.0", "verb": "workflow-run",
				"error": "workflow_refused", "message": rerr.Error()})
			fmt.Fprintln(stdout, string(b))
			return 3
		}
		b, _ := json.Marshal(map[string]any{"ok": res.Status != "failed", "schema_version": "1.0", "verb": "workflow-run",
			"run_id": res.RunID, "status": res.Status, "run_dir": res.RunDir,
			"steps": res.Steps, "notes": res.Notes, "next_steps": res.NextSteps})
		fmt.Fprintln(stdout, string(b))
		if res.Status == "failed" {
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "seed: unknown workflow subcommand %q\n", args[0])
		return exitUsage
	}
}

type inputFlags struct{ m map[string]string }

func (f *inputFlags) String() string { return "" }
func (f *inputFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("--input wants k=v, got %q", v)
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[k] = val
	return nil
}
