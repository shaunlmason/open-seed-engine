// Command seed is the open-seed protocol engine: the pinned binary that the
// template's bootstrap shim (scripts/seed) downloads, verifies, and execs.
// Every port verb emits one JSON envelope on stdout and a port exit code
// (.seed/port-schema/port.json); CLI usage errors exit 64.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/backend"
	"github.com/shaunlmason/open-seed-engine/internal/config"
	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/plan"
	"github.com/shaunlmason/open-seed-engine/internal/prclass"
	"github.com/shaunlmason/open-seed-engine/internal/receipt"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	seedsync "github.com/shaunlmason/open-seed-engine/internal/sync"
	"github.com/shaunlmason/open-seed-engine/internal/task"
	"github.com/shaunlmason/open-seed-engine/internal/upgrade"
	"github.com/shaunlmason/open-seed-engine/internal/validate"
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
	case "plan":
		return runPlan(args[1:], stdout, stderr)
	case "pr":
		return runPR(args[1:], stdout, stderr)
	case "receipt":
		return runReceipt(args[1:], stdout, stderr)
	case "validate":
		return runValidate(stdout, stderr)
	case "sync":
		return runSync(args[1:], stdout, stderr)
	case "backend":
		return runBackend(args[1:], stdout, stderr)
	case "upgrade":
		return runUpgrade(args[1:], stdout, stderr)
	case "template":
		return runTemplate(args[1:], stdout, stderr)
	case "workflow":
		return runWorkflow(args[1:], stdout, stderr)
	case "skills":
		return runSkills(args[1:], stdout, stderr)
	case "plugin":
		return runPlugin(args[1:], stdout, stderr)
	case "mail":
		return runMail(args[1:], stdout, stderr)
	case "handoff":
		return runHandoff(args[1:], stdout, stderr)
	case "mcp":
		return runMCP(args[1:], stdout, stderr)
	case "init":
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.Init() })
	case "init-github":
		return runInitGithub(stdout, stderr)
	case "state":
		return runState(args[1:], stdout, stderr)
	case "maintain":
		return runMaintain(args[1:], stdout, stderr)
	case "mirror":
		return runMirror(args[1:], stdout, stderr)
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
  plan lint <path>             lint one plan file against the D3 grammar
  pr classify <branch> [--files a,b,c]   classify a PR + check purity (D3)
  receipt generate <task> --base <ref> [--run] [--by <name>] [--write]
  receipt verify <task> --base <ref> --branch <head-branch> [--run] [--write]
                               [--emit <path>] [--by <ci-run>]
  receipt migrate <task>|--all rewrite committed receipts in the current schema
  validate                     lint guardrails, teams, role variants, plans
  backend verify <name>        manifest + lock-hash check for a backend plugin
  sync [--check]               regenerate fan-outs (.claude/agents|skills,
                               .agents/skills, AGENTS.md rules block); --check
                               fails on drift (offline; runs in CI — R1)
  upgrade [--to vX.Y.Z] [--check] [--assume-protocol-ok]
                               move the engine pin in .seed/engine.lock against
                               a tagged release (verified checksums + protocol
                               preflight; never touches git — exit map 0 ok,
                               1 refusal, 7 release host unreachable)
  template upgrade [--to vX.Y.Z] [--check]
                               three-way merge a newer template release onto a
                               new local branch template-upgrade/<tag> (base
                               recorded in .seed/template.lock; work products
                               never merged; no push, no PR — exit map 0 ok,
                               1 refusal, 7 template host unreachable)
  workflow validate [<file>|--all] [--with-harnesses]
                               preflight checked-in workflow DAGs (the thirteen
                               rules; any finding exits 3)
  workflow run <name> [--input k=v]... [--mock] [--resume <id>]
                               execute a workflow in parallel waves; --mock is
                               zero-credential AND zero-side-effect; run state
                               under <git-common-dir>/seed-runs/<run-id>/
  plugin enable [--ref R]      opt this repo into the Claude Code plugin channel
                               by composing the project-scope settings
                               declaration (§10 Q4; control surface, so the edit
                               lands via a reviewed PR). --ref tracks a later
                               release or a branch: a capability-only update
  plugin disable               opt out of the Claude Code plugin channel
  plugin status [--check]      report both channels' release coordinates and
                               how they stand: off, aligned, ahead, floating,
                               behind, unpinned. --check exits 1 on behind (a
                               stale pin) and unpinned (nothing usable to
                               compare); off, aligned, ahead and floating all
                               pass, so a deliberate capability-only ref never
                               fails the gate (offline)
  skills lock                  resolve seed.yaml sources and pin seed.lock
  skills install [--frozen]    materialize pinned skills into skills/managed/
                               (--frozen refuses unlocked edits and drift — CI)
  mail send|read|ack|nudge|prune   one-file-per-message mailboxes on the
                               state ref (ack = move; nudge = content-free
                               tmux poke, no-op without tmux)
  handoff generate <task> [--write]   render the mechanical continuation
                               packet (card + git anchors) to handoff/<task>.md
  mcp serve                    MCP stdio transport: one tool per port verb,
                               same service path as the CLI (which stays the
                               source of truth)
  init                         create the seed-state ref (orphan; race-safe)
  init-github                  print the server-side protection checklist
  state resume --actor A       clear the HALT marker (operator)
  state lint [--halt-on-fail] [--actor A]   card lint + done-consistency +
                               transition-table replay; failure writes HALT
  state anchor                 tag the state head as seed-anchor/<ts> and push
  state export                 dump the whole store as one JSON document
  state import <file|->        load an export into a fresh store (operator)
  maintain reap --actor A      release every expired lease (handoff stubs)
  maintain report              per-state counts, expired leases, stalled reviews
  mirror plan                  compute one-way issue-export actions (read-only)
  mirror record <id> --issue N --state S --actor A   store a card↔issue mapping
  task <verb> ...              port verbs (JSON envelope on stdout):
    create --title T [--body B] [--priority P2] [--squad S] [--parent ID]
           [--label L]... [--blocks ID]... [--blocked-by ID]... --actor A
    ready [--actor A] [--squad S]
    get <id> | list [--state s]
    claim <id> --actor A [--lease 45m]
    transition <id> --to STATE --actor A [--token T] [--blocked-on entry]
    release <id> --actor A --token T
    accept|reject|cancel|promote|deprioritize|block|unblock|reinstate|close <id>
           --actor A [--resolution MSG] [--blocked-on entry] [--no-pr]
    record-evidence <id> --actor A --resolution URL [--no-pr]
                                 complete an accept that recorded none
                                 (operator; only when the review block is
                                 accepted and its evidence is empty)
    plan-unblock <id> --pr N --actor A     remove a plan:<N> entry (operator;
           the caller has established the PR is merged or closed)
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

// runUpgrade is not a port verb: it has its own exit map (0/1/7/64) and
// bypasses the task service (it must run even when the pinned engine and
// spec would disagree: that disagreement is what it fixes).
func runUpgrade(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "explicit target tag (permits rollback)")
	check := fs.Bool("check", false, "report current vs target without writing")
	assume := fs.Bool("assume-protocol-ok", false, "proceed when the release predates protocol.txt")
	if fs.Parse(args) != nil {
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
	res, uerr := upgrade.Run(upgrade.Options{
		Root: root, To: *to, Check: *check, AssumeProtocolOK: *assume,
		BaseURL: os.Getenv("SEED_UPGRADE_BASE_URL"),
	})
	if uerr != nil {
		b, _ := json.Marshal(map[string]any{"ok": false, "schema_version": "1.0", "verb": "upgrade",
			"error": uerr.Name, "message": uerr.Msg})
		fmt.Fprintln(stdout, string(b))
		return uerr.Code
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "schema_version": "1.0", "verb": "upgrade",
		"current": res.Current, "target": res.Target, "up_to_date": res.UpToDate,
		"written": res.Written, "release_url": res.ReleaseURL,
		"notes": res.Notes, "next_steps": res.NextSteps})
	fmt.Fprintln(stdout, string(b))
	return 0
}

func runState(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	fs := flag.NewFlagSet("state "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	actor := fs.String("actor", "maintenance", "acting identity")
	haltOnFail := fs.Bool("halt-on-fail", false, "write the HALT marker on conformance failure")
	if fs.Parse(args[1:]) != nil {
		return exitUsage
	}
	switch args[0] {
	case "resume":
		if *actor == "" {
			return exitUsage
		}
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.Resume(*actor) })
	case "lint":
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.StateLint(*haltOnFail, *actor) })
	case "anchor":
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.Anchor() })
	case "export":
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.Export() })
	case "import":
		rest := fs.Args()
		var raw []byte
		var err error
		if len(rest) == 0 || rest[0] == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(rest[0])
		}
		if err != nil {
			fmt.Fprintf(stderr, "seed state import: %v\n", err)
			return exitUsage
		}
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.Import(raw, *actor) })
	default:
		usage(stderr)
		return exitUsage
	}
}

func runMaintain(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	fs := flag.NewFlagSet("maintain "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	actor := fs.String("actor", "", "operator actor")
	stalled := fs.Duration("stalled-after", 48*time.Hour, "review/parked age considered stalled")
	if fs.Parse(args[1:]) != nil {
		return exitUsage
	}
	switch args[0] {
	case "reap":
		if *actor == "" {
			fmt.Fprintln(stderr, "seed maintain reap: --actor required (must be in the operator roster)")
			return exitUsage
		}
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.ReapExpired(*actor) })
	case "report":
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.Report(*stalled) })
	default:
		usage(stderr)
		return exitUsage
	}
}

func runMirror(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "plan":
		return withService(stdout, stderr, func(sv *task.Service) *task.Result { return sv.MirrorPlan() })
	case "record":
		if len(args) < 2 {
			usage(stderr)
			return exitUsage
		}
		id := args[1]
		fs := flag.NewFlagSet("mirror record", flag.ContinueOnError)
		fs.SetOutput(stderr)
		issue := fs.Int("issue", 0, "issue number")
		state := fs.String("state", "", "card state at export")
		actor := fs.String("actor", "", "operator actor")
		if fs.Parse(args[2:]) != nil || *issue == 0 || *state == "" || *actor == "" {
			fmt.Fprintln(stderr, "seed mirror record: <id> --issue N --state S --actor A required")
			return exitUsage
		}
		return withService(stdout, stderr, func(sv *task.Service) *task.Result {
			return sv.MirrorRecord(id, *issue, *state, *actor)
		})
	default:
		usage(stderr)
		return exitUsage
	}
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
	// External-backend dispatch (§7.1): when the configured backend's manifest
	// entry is not builtin, every task verb is executed by the plugin: the
	// shim only verifies, sandboxes, and validates.
	if cwd, err := os.Getwd(); err == nil {
		if root, found := config.FindRoot(cwd); found {
			if cfg, err := config.Load(filepath.Join(root, ".seed")); err == nil && cfg.Coordination.Backend != "" {
				if m, err := backend.Load(root, cfg.Coordination.Backend); err == nil && m.Entry != "builtin" {
					out, code, execErr := backend.Exec(root, m, append(slicesClone(args), "--json"))
					if execErr != nil {
						fmt.Fprintln(stderr, "seed task:", execErr)
						return code
					}
					fmt.Fprint(stdout, out)
					return code
				} else if err != nil && cfg.Coordination.Backend != "filecards" {
					fmt.Fprintln(stderr, "seed task:", err)
					return spec.ExitUnavailable
				}
			}
		}
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
	pr := fs.Int("pr", 0, "")
	noPR := fs.Bool("no-pr", false, "")
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
		case "plan-unblock":
			return sv.PlanUnblock(id, *pr, *actor)
		case "comment":
			return sv.Append("comment", id, *actor, *token, *body, "")
		case "attach-evidence":
			return sv.Append(*kind, id, *actor, *token, "", *ref)
		case "lease-renew":
			return sv.LeaseRenew(id, *actor, *token, *lease)
		case "record-evidence":
			return sv.RecordEvidence(id, *actor, *resolution, *noPR)
		default:
			if operatorVerbs[verb] {
				return sv.Transition(task.TransitionArgs{Verb: verb, ID: id, To: *to,
					Actor: *actor, Token: *token, BlockedOn: *blockedOn, Resolution: *resolution, NoPR: *noPR})
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

func runPlan(args []string, stdout, stderr *os.File) int {
	if len(args) < 2 || args[0] != "lint" {
		usage(stderr)
		return exitUsage
	}
	b, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(stderr, "seed plan lint:", err)
		return 1
	}
	if errs := plan.Lint(string(b)); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(stderr, "seed plan lint: %s: %v\n", args[1], e)
		}
		return 1
	}
	p := plan.Parse(string(b))
	fmt.Fprintf(stdout, "plan ok: %d validation commands, sha256 %s\n", len(p.ValidationCommands), plan.Hash(string(b)))
	return 0
}

func runPR(args []string, stdout, stderr *os.File) int {
	if len(args) < 2 || args[0] != "classify" {
		usage(stderr)
		return exitUsage
	}
	branch := args[1]
	fs := flag.NewFlagSet("pr classify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	filesArg := fs.String("files", "", "comma-separated changed files for the purity check")
	if fs.Parse(args[2:]) != nil {
		return exitUsage
	}
	kind, taskID := prclass.Classify(branch)
	fmt.Fprintf(stdout, "class=%s task=%s\n", kind, taskID)
	if *filesArg != "" {
		if err := prclass.CheckPurity(kind, taskID, strings.Split(*filesArg, ",")); err != nil {
			fmt.Fprintln(stderr, "seed pr classify:", err)
			return 1
		}
		fmt.Fprintln(stdout, "purity ok")
	}
	return 0
}

func runReceipt(args []string, stdout, stderr *os.File) int {
	if len(args) < 2 {
		usage(stderr)
		return exitUsage
	}
	// `receipt migrate --all` names no task, so the id is optional when the
	// next argument is already a flag.
	sub, taskID, rest := args[0], "", args[1:]
	if !strings.HasPrefix(args[1], "-") {
		taskID, rest = args[1], args[2:]
	}
	fs := flag.NewFlagSet("receipt "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "main", "base ref (the default branch)")
	branch := fs.String("branch", "", "PR head branch (verify)")
	runVal := fs.Bool("run", false, "execute the merge-base plan's validation commands")
	write := fs.Bool("write", false, "write the regenerated claim to receipts/<task>.json")
	emit := fs.String("emit", "", "write the full attestation (claim + verified snapshot) to this path")
	all := fs.Bool("all", false, "migrate: every receipt under receipts/")
	by := fs.String("by", "local", "generated_by identity")
	if fs.Parse(rest) != nil {
		return exitUsage
	}
	cwd, _ := os.Getwd()
	root, found := config.FindRoot(cwd)
	if !found {
		root = cwd
	}
	if taskID != "" {
		if err := receipt.ValidTaskID(taskID); err != nil {
			fmt.Fprintln(stderr, "seed receipt "+sub+":", err)
			return exitUsage
		}
	}
	// The attestation is a per-run finding, not tree content: emitting one
	// into receipts/ would leave a file that reads as a claim.
	if *emit != "" && receipt.UnderReceipts(root, *emit) {
		fmt.Fprintln(stderr, "seed receipt "+sub+": refusing to emit the attestation into receipts/ — that directory holds committed claims, and an attestation is stale the moment it lands there")
		return exitUsage
	}
	repo := &gitx.Repo{Dir: root}
	opts := receipt.Options{RunValidation: *runVal, GeneratedBy: *by}

	switch sub {
	case "migrate":
		return runReceiptMigrate(root, taskID, *all, stdout, stderr)
	case "generate":
		r, err := receipt.Generate(repo, taskID, *base, opts)
		if err != nil {
			fmt.Fprintln(stderr, "seed receipt generate:", err)
			return 1
		}
		if *write {
			if err := r.WriteClaim(root); err != nil {
				fmt.Fprintln(stderr, "seed receipt generate:", err)
				return 1
			}
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0
	case "verify":
		if *branch == "" {
			fmt.Fprintln(stderr, "seed receipt verify: --branch required (the PR head branch; merge-queue callers derive it from the PR, never the merge-group ref)")
			return exitUsage
		}
		opts.VerifiedBy = *by
		rep, err := receipt.Verify(repo, taskID, *base, *branch, opts)
		if err != nil {
			fmt.Fprintln(stderr, "seed receipt verify:", err)
			return 1
		}
		// The attestation is emitted whether or not the gate passed: a red
		// run must still say what it verified against.
		if rep.Regenerated != nil {
			if *write {
				if err := rep.Regenerated.WriteClaim(root); err != nil {
					fmt.Fprintln(stderr, "seed receipt verify:", err)
					return 1
				}
			}
			if *emit != "" {
				if err := rep.Regenerated.WriteAttestation(*emit); err != nil {
					fmt.Fprintln(stderr, "seed receipt verify:", err)
					return 1
				}
			}
		}
		if !rep.OK {
			for _, f := range rep.Failures {
				fmt.Fprintln(stderr, "seed receipt verify: FAIL:", f)
			}
			return 1
		}
		fmt.Fprintf(stdout, "verify ok: plan %s, diff %s (%d files)\n",
			rep.Regenerated.PlanSHA256[:12], rep.Regenerated.DiffSHA256[:12], len(rep.Regenerated.DiffFiles))
		return 0
	default:
		usage(stderr)
		return exitUsage
	}
}

// runReceiptMigrate rewrites committed receipts in the current schema,
// dropping the merge base, head and diff a 1.0 file carried inline. Those
// fields described the branch at one instant and were re-verified from git on
// every run anyway; the file that remains is the durable claim.
func runReceiptMigrate(root, taskID string, all bool, stdout, stderr *os.File) int {
	var paths []string
	switch {
	case all && taskID != "":
		fmt.Fprintln(stderr, "seed receipt migrate: pass a task id or --all, not both")
		return exitUsage
	case all:
		matches, err := filepath.Glob(filepath.Join(root, "receipts", "*.json"))
		if err != nil {
			fmt.Fprintln(stderr, "seed receipt migrate:", err)
			return 1
		}
		sort.Strings(matches)
		paths = matches
	case taskID != "":
		paths = []string{receipt.Path(root, taskID)}
	default:
		fmt.Fprintln(stderr, "seed receipt migrate: pass a task id or --all")
		return exitUsage
	}
	migrated := 0
	for _, p := range paths {
		changed, err := receipt.Migrate(p)
		if err != nil {
			fmt.Fprintln(stderr, "seed receipt migrate:", err)
			return 1
		}
		if changed {
			migrated++
			fmt.Fprintln(stdout, "migrated", filepath.Base(p))
		}
	}
	fmt.Fprintf(stdout, "migrate: %d of %d receipts rewritten at schema %s\n", migrated, len(paths), receipt.SchemaVersion)
	return 0
}

func runValidate(stdout, stderr *os.File) int {
	cwd, _ := os.Getwd()
	root, found := config.FindRoot(cwd)
	if !found {
		fmt.Fprintln(stderr, "seed validate: no .seed directory found")
		return 1
	}
	if errs := validate.Repo(root); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(stderr, "seed validate:", e)
		}
		return 1
	}
	// §6 advisories (report, not refusal): CODEOWNERS binding under
	// multi-squad, and goal ancestry once >1 squad or a mission exists.
	warns := validate.TeamsWarnings(root)
	if teams, _ := validate.LoadTeams(root); validate.AncestryActive(teams) {
		if sv, err := task.NewService(root); err == nil {
			warns = append(warns, sv.AncestryWarnings(teams)...)
		}
	}
	for _, w := range warns {
		fmt.Fprintln(stderr, "seed validate: warning:", w)
	}
	fmt.Fprintln(stdout, "validate ok: guardrails, teams, role variants, plans")
	return 0
}

func slicesClone(s []string) []string { return append([]string{}, s...) }

// runBackend implements `seed backend verify <name>`: manifest + lock check
// without invoking the plugin.
func runBackend(args []string, stdout, stderr *os.File) int {
	if len(args) < 2 || args[0] != "verify" {
		usage(stderr)
		return exitUsage
	}
	cwd, _ := os.Getwd()
	root, found := config.FindRoot(cwd)
	if !found {
		fmt.Fprintln(stderr, "seed backend verify: no .seed directory found")
		return 1
	}
	name := args[1]
	m, err := backend.Load(root, name)
	if err != nil {
		fmt.Fprintln(stderr, "seed backend verify:", err)
		return 1
	}
	if err := backend.VerifyLock(root, name); err != nil {
		fmt.Fprintln(stderr, "seed backend verify:", err)
		return 1
	}
	fmt.Fprintf(stdout, "backend %s ok: entry=%s schema=%s atomic_claim=%s offline=%s\n",
		m.Name, m.Entry, m.SchemaVersion, m.Capabilities.AtomicClaim, m.Capabilities.Offline)
	return 0
}

func runSync(args []string, stdout, stderr *os.File) int {
	check := len(args) > 0 && args[0] == "--check"
	cwd, _ := os.Getwd()
	root, found := config.FindRoot(cwd)
	if !found {
		fmt.Fprintln(stderr, "seed sync: no .seed directory found")
		return 1
	}
	if check {
		if errs := seedsync.Check(root); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintln(stderr, "seed sync --check:", e)
			}
			return 1
		}
		fmt.Fprintln(stdout, "sync ok: fan-outs match their sources")
		return 0
	}
	n, err := seedsync.Apply(root)
	if err != nil {
		fmt.Fprintln(stderr, "seed sync:", err)
		return 1
	}
	fmt.Fprintf(stdout, "sync ok: %d file(s) written\n", n)
	return 0
}

func runInitGithub(stdout, stderr *os.File) int {
	fmt.Fprint(stdout, `seed init-github — the server-side protections that make the gates real:

  1. Branch protection on 'main' (the merge gates):
     - require the check-validate checks ('check', 'verify') and a review
     - require conversation resolution before merging — review threads
       must be resolved before merge, so review fixes cannot be stranded
       behind an early merge
     - no force pushes
  2. Branch protection / ruleset on 'seed-state':
     - allow pushes (contributors by default; see hardening below)
     - BLOCK force pushes and deletion
  3. Tag protection rule for 'seed-anchor/*':
     - create-only for the maintenance workflow credential; no deletion
  4. Hardening option (Q5): restrict 'seed-state' pushes to a dedicated
     machine identity (fine-grained PAT or deploy key) plus squad leads.
  5. Never grant any scheduled job push access to the default branch (D4.3).

The engine cannot call the GitHub API; apply these in repo settings, then
verify the ref exists with: git ls-remote origin seed-state
`)
	return 0
}
