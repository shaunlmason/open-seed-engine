package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/shaunlmason/open-seed-engine/internal/config"
	"github.com/shaunlmason/open-seed-engine/internal/skills"
)

// runSkills: `seed skills lock|install [--frozen]` (plan os-6f3104db).
// Not a port verb: skill content is control-surface material moved by
// reviewed PRs; these commands only resolve pins and materialize them.
func runSkills(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "seed: usage: seed skills lock | seed skills install [--frozen]")
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
	fail := func(verb string, err error) int {
		b, _ := json.Marshal(map[string]any{"ok": false, "schema_version": "1.0", "verb": verb,
			"error": "skills_refused", "message": err.Error()})
		fmt.Fprintln(stdout, string(b))
		return 1
	}
	switch args[0] {
	case "lock":
		lock, err := skills.LockAll(root)
		if err != nil {
			return fail("skills-lock", err)
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "schema_version": "1.0", "verb": "skills-lock",
			"skills": lock.Skills, "next_steps": []string{"review the seed.lock diff (skill content arrives via `seed skills install` and lands in the PR for injection review)", "commit seed.lock"}})
		fmt.Fprintln(stdout, string(b))
		return 0
	case "install":
		frozen := len(args) > 1 && args[1] == "--frozen"
		rep, err := skills.Install(root, frozen)
		if err != nil {
			return fail("skills-install", err)
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "schema_version": "1.0", "verb": "skills-install",
			"installed": rep.Installed, "pruned": rep.Pruned, "frozen": rep.Frozen})
		fmt.Fprintln(stdout, string(b))
		return 0
	default:
		fmt.Fprintf(stderr, "seed: unknown skills subcommand %q\n", args[0])
		return exitUsage
	}
}
