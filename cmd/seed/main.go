// Command seed is the open-seed protocol engine: the pinned binary that the
// template's bootstrap shim (scripts/seed) downloads, verifies, and execs.
//
// Phase 0 ships only `seed version`, proving the release pipeline end-to-end.
// The port verbs land in Phase 1 (see open-seed docs/build-plan.md).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaunlmason/open-seed-engine/internal/spec"
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
	case "spec":
		return runSpec(args[1:], stdout, stderr)
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
  version              print the engine version
  spec lint [dir]      load and validate the port spec (default .seed/port-schema);
                       also checks .seed/version against the spec's protocol_version

The remaining port verbs (task, receipt, sync, backend, hooks, init, state)
arrive in later phases of the open-seed build plan.
`)
}

// runSpec implements `seed spec lint [dir]`. A structurally invalid spec or a
// protocol-version mismatch exits 10 (the port's version_mismatch class); a
// valid spec prints a summary and exits 0.
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
