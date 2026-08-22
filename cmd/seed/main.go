// Command seed is the open-seed protocol engine: the pinned binary that the
// template's bootstrap shim (scripts/seed) downloads, verifies, and execs.
// Every port verb emits one JSON envelope on stdout and a port exit code
// (.seed/port-schema/port.json); CLI usage errors exit 64.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaunlmason/open-seed-engine/internal/config"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	"github.com/shaunlmason/open-seed-engine/internal/task"
)

// Injected by goreleaser via -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// exitUsage is EX_USAGE from sysexits; the port's reserved verb exit codes
// (0, 2, 3, 4, 5, 6, 10) must never be emitted for CLI usage errors.
const exitUsage = 64

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "seed %s (commit %s, built %s)\n", version, commit, date)
		return 0
	case "spec":
		return runSpec(args[1:], stdout, stderr)
	case "init":
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.Init() })
	case "init-github":
		return runInitGithub(stdout, stderr)
	case "state":
		if len(args) < 2 || args[1] != "resume" {
			usage(stderr)
			return exitUsage
		}
		fs := flag.NewFlagSet("state resume", flag.ContinueOnError)
		actor := fs.String("actor", "", "operator actor")
		if fs.Parse(args[2:]) != nil || *actor == "" {
			return exitUsage
		}
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.Resume(*actor) })
	case "task":
		return runTask(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "seed: unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage: seed <command>

commands:
  version                      print the engine version
  spec lint [dir]              validate the port spec (exit 10 on mismatch)
  init                         create the seed-state ref (orphan; race-safe)
  init-github                  print the server-side protection checklist
  state resume --actor A       clear the HALT marker (operator)
  task <verb> ...              port verbs (JSON envelope on stdout):
    create --title T [--body B] [--priority P2] [--squad S] [--parent ID]
           [--label L]... [--blocks ID]... [--blocked-by ID]... --actor A
    ready [--actor A] [--squad S]
    get <id> | list [--state s]
    claim <id> --actor A [--lease 45m]
    transition <id> --to STATE --actor A [--token T] [--blocked-on entry]
    release <id> --actor A --token T
    accept|reject|cancel|promote|deprioritize|block|unblock|reinstate|close <id>
           --actor A [--resolution MSG] [--blocked-on entry]
    comment <id> --actor A --body B [--token T]
    attach-evidence <id> --actor A --kind K --ref R [--token T]
    lease-renew <id> --actor A --token T [--lease 45m]
`)
}

func emit(stdout *os.File, r *task.Result) int {
	env := map[string]any{"ok": r.Code == 0, "schema_version": "1.0"}
	for k, v := range r.Fields {
		env[k] = v
	}
	if r.Err != "" {
		env["error"] = r.Err
	}
	b, _ := json.Marshal(env)
	fmt.Fprintln(stdout, string(b))
	return r.Code
}

func withService(stdout, stderr *os.File, f func(*task.Service) *task.Result) int {
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
	sv, err := task.NewService(root)
	if err != nil {
		if vm, ok := err.(*spec.VersionMismatch); ok {
			return emit(stdout, &task.Result{Code: spec.ExitVersionMismatch, Err: "version_mismatch",
				Fields: map[string]any{"message": vm.Error()}})
		}
		fmt.Fprintln(stderr, "seed:", err)
		return spec.ExitVersionMismatch
	}
	return emit(stdout, f(sv))
}

var operatorVerbs = map[string]bool{
	"accept": true, "reject": true, "cancel": true, "promote": true,
	"deprioritize": true, "block": true, "unblock": true, "reinstate": true,
	"close": true,
}

func runTask(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	verb := args[0]
	rest := args[1:]

	// Verbs taking a positional <id> before flags.
	id := ""
	needsID := verb != "create" && verb != "ready" && verb != "list"
	if needsID {
		if len(rest) == 0 || len(rest[0]) == 0 || rest[0][0] == '-' {
			fmt.Fprintf(stderr, "seed task %s: task id required\n", verb)
			return exitUsage
		}
		id, rest = rest[0], rest[1:]
	}

	fs := flag.NewFlagSet("task "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var labels, blocks, blockedBy multiFlag
	title := fs.String("title", "", "")
	body := fs.String("body", "", "")
	priority := fs.String("priority", "", "")
	squad := fs.String("squad", "", "")
	parent := fs.String("parent", "", "")
	actor := fs.String("actor", "", "")
	lease := fs.String("lease", "", "")
	token := fs.String("token", "", "")
	to := fs.String("to", "", "")
	blockedOn := fs.String("blocked-on", "", "")
	resolution := fs.String("resolution", "", "")
	state := fs.String("state", "", "")
	kind := fs.String("kind", "", "")
	ref := fs.String("ref", "", "")
	fs.Var(&labels, "label", "")
	fs.Var(&blocks, "blocks", "")
	fs.Var(&blockedBy, "blocked-by", "")
	_ = fs.Bool("json", true, "always on")
	if fs.Parse(rest) != nil {
		return exitUsage
	}

	return withService(stdout, stderr, func(sv *task.Service) *task.Result {
		switch verb {
		case "create":
			if *title == "" {
				return &task.Result{Code: exitUsage, Err: "title_required"}
			}
			return sv.Create(task.CreateArgs{Title: *title, Body: *body, Priority: *priority,
				Squad: *squad, Parent: *parent, Actor: *actor,
				Labels: labels, Blocks: blocks, BlockedBy: blockedBy})
		case "ready":
			return sv.Ready(*actor, *squad)
		case "get":
			return sv.Get(id)
		case "list":
			return sv.List(*state)
		case "claim":
			return sv.Claim(id, *actor, *lease)
		case "transition", "release":
			return sv.Transition(task.TransitionArgs{Verb: verb, ID: id, To: *to,
				Actor: *actor, Token: *token, BlockedOn: *blockedOn, Resolution: *resolution})
		case "comment":
			return sv.Append("comment", id, *actor, *token, *body, "")
		case "attach-evidence":
			return sv.Append(*kind, id, *actor, *token, "", *ref)
		case "lease-renew":
			return sv.LeaseRenew(id, *actor, *token, *lease)
		default:
			if operatorVerbs[verb] {
				return sv.Transition(task.TransitionArgs{Verb: verb, ID: id, To: *to,
					Actor: *actor, Token: *token, BlockedOn: *blockedOn, Resolution: *resolution})
			}
			return &task.Result{Code: exitUsage, Err: "unknown_verb"}
		}
	})
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runSpec(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "lint" {
		usage(stderr)
		return exitUsage
	}
	dir := filepath.Join(".seed", "port-schema")
	if len(args) > 1 {
		dir = args[1]
	}
	s, err := spec.Load(dir)
	if err != nil {
		fmt.Fprintf(stderr, "seed spec lint: %v\n", err)
		return spec.ExitVersionMismatch
	}
	seedDir := filepath.Dir(dir)
	if _, statErr := os.Stat(filepath.Join(seedDir, "version")); statErr == nil {
		if err := spec.CheckVersion(s, seedDir); err != nil {
			fmt.Fprintf(stderr, "seed spec lint: %v\n", err)
			return spec.ExitVersionMismatch
		}
	}
	fmt.Fprintf(stdout, "spec ok: protocol %d, %d states, %d transitions, %d composite verbs\n",
		s.Port.ProtocolVersion, len(s.Port.States), len(s.Table.Transitions), len(s.Table.CompositeVerbs))
	return 0
}

func runInitGithub(stdout, stderr *os.File) int {
	fmt.Fprint(stdout, `seed init-github — server-side protections for the seed-state ref (§7.2):

  1. Branch protection / ruleset on 'seed-state':
     - allow pushes (contributors by default; see hardening below)
     - BLOCK force pushes and deletion
  2. Tag protection rule for 'seed-anchor/*':
     - create-only for the maintenance workflow credential; no deletion
  3. Hardening option (Q5): restrict 'seed-state' pushes to a dedicated
     machine identity (fine-grained PAT or deploy key) plus squad leads.
  4. Never grant any scheduled job push access to the default branch (D4.3).

The engine cannot call the GitHub API; apply these in repo settings, then
verify the ref exists with: git ls-remote origin seed-state
`)
	return 0
}
