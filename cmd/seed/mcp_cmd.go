package main

import (
	"fmt"
	"os"

	"github.com/shaunlmason/open-seed-engine/internal/config"
	"github.com/shaunlmason/open-seed-engine/internal/mcptransport"
	"github.com/shaunlmason/open-seed-engine/internal/task"
)

// runMCP: `seed mcp serve` (plan os-67a1bf14) — the v2 transport: MCP
// over stdio, one tool per port verb, dispatching through the identical
// service path the CLI uses. The CLI stays the source of truth.
func runMCP(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(stderr, "seed: usage: seed mcp serve")
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
	if err := mcptransport.Serve(os.Stdin, stdout, func() (*task.Service, error) { return task.NewService(root) }); err != nil {
		fmt.Fprintln(stderr, "seed mcp:", err)
		return 1
	}
	return 0
}
