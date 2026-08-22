// Command seed is the open-seed protocol engine: the pinned binary that the
// template's bootstrap shim (scripts/seed) downloads, verifies, and execs.
//
// Phase 0 ships only `seed version`, proving the release pipeline end-to-end.
// The port verbs land in Phase 1 (see open-seed docs/build-plan.md).
package main

import (
	"fmt"
	"os"
)

// Injected by goreleaser via -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// exitUsage is EX_USAGE from sysexits; the port's reserved verb exit codes
// (0, 2, 3, 6, 10) must never be emitted for CLI usage errors.
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
		fmt.Fprintln(stdout, versionString())
		return 0
	default:
		fmt.Fprintf(stderr, "seed: unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func versionString() string {
	return fmt.Sprintf("seed %s (commit %s, built %s)", version, commit, date)
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage: seed <command>

commands:
  version    print the engine version

The port verbs (task, receipt, sync, backend, hooks, init, state) arrive in
Phase 1+ of the open-seed build plan.
`)
}
