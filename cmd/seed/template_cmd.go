package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/shaunlmason/open-seed-engine/internal/config"
	"github.com/shaunlmason/open-seed-engine/internal/template"
)

// runTemplate: `seed template upgrade` — pull-based template updates (plan
// os-23494e11). Deliberately bypasses withService: it is not a port verb,
// touches no coordination state, and must work while the backend is halted.
func runTemplate(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "upgrade" {
		fmt.Fprintln(stderr, "seed: usage: seed template upgrade [--to vX.Y.Z] [--check]")
		return exitUsage
	}
	fs := flag.NewFlagSet("template upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "explicit target tag (permits rollback)")
	check := fs.Bool("check", false, "report current vs target without fetching or branching")
	if fs.Parse(args[1:]) != nil {
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
	res, terr := template.Run(template.Options{
		Root: root, To: *to, Check: *check,
		BaseURL: os.Getenv("SEED_UPGRADE_BASE_URL"),
		GitURL:  os.Getenv("SEED_TEMPLATE_GIT_URL"),
	})
	if terr != nil {
		b, _ := json.Marshal(map[string]any{"ok": false, "schema_version": "1.0", "verb": "template-upgrade",
			"error": terr.Name, "message": terr.Msg})
		fmt.Fprintln(stdout, string(b))
		return terr.Code
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "schema_version": "1.0", "verb": "template-upgrade",
		"current": res.Current, "target": res.Target, "up_to_date": res.UpToDate,
		"branch": res.Branch, "commit": res.Commit,
		"merged": res.Merged, "conflicts": res.Conflicts, "deletions": res.Deletions,
		"notes": res.Notes, "next_steps": res.NextSteps})
	fmt.Fprintln(stdout, string(b))
	return 0
}
