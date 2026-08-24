package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/shaunlmason/open-seed-engine/internal/config"
	"github.com/shaunlmason/open-seed-engine/internal/plugin"
)

// runPlugin: `seed plugin enable|disable|status [--check]` (plan
// os-221f5929, §10 Q4). Not a port verb and never a network call: these
// commands only compose the project-scope settings declaration that opts a
// repo into the plugin channel, and report whether that declaration still
// names the release .seed/template.lock does.
//
// The plugin PAYLOAD is not built here: it is a generated fan-out that
// `seed sync` writes and `seed sync --check` polices, so the channel shares
// one drift-detection story with every other fan-out.
func runPlugin(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "seed: usage: seed plugin enable | seed plugin disable | seed plugin status [--check]")
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
	emit := func(verb string, s *plugin.Status, next []string) int {
		b, _ := json.Marshal(map[string]any{"ok": true, "schema_version": "1.0", "verb": verb,
			"enabled": s.Enabled, "pinned_ref": s.PinnedRef, "pinned_repo": s.PinnedRepo,
			"template_repo": s.TemplateRepo, "template_version": s.TemplateVersion,
			"relation": string(s.Relation),
			"drifted":  s.Drifted, "detail": s.Detail, "next_steps": next})
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fail := func(verb string, err error) int {
		b, _ := json.Marshal(map[string]any{"ok": false, "schema_version": "1.0", "verb": verb,
			"error": "plugin_refused", "message": err.Error()})
		fmt.Fprintln(stdout, string(b))
		return 1
	}

	// Unknown or extra arguments are a usage error, never a silent no-op:
	// `seed plugin status --chek` must not pass as a clean drift check in
	// somebody's CI.
	ref := ""
	switch {
	case args[0] == "status" && len(args) == 2 && args[1] == "--check":
	case args[0] == "enable" && len(args) == 3 && args[1] == "--ref" && args[2] != "":
		ref = args[2]
	case len(args) > 1:
		fmt.Fprintf(stderr, "seed plugin %s: unexpected argument %q\n", args[0], args[1])
		return exitUsage
	}

	switch args[0] {
	case "enable":
		s, err := plugin.Enable(root, ref)
		if err != nil {
			return fail("plugin-enable", err)
		}
		return emit("plugin-enable", s, []string{
			"review the " + plugin.SettingsPath + " diff (control surface: D4.1 requires an owner's review)",
			"commit it; Claude Code registers the marketplace once the folder is trusted",
		})
	case "disable":
		s, err := plugin.Disable(root)
		if err != nil {
			return fail("plugin-disable", err)
		}
		return emit("plugin-disable", s, []string{
			"review and commit the " + plugin.SettingsPath + " diff",
		})
	case "status":
		check := len(args) == 2
		s, err := plugin.Report(root)
		if err != nil {
			return fail("plugin-status", err)
		}
		if check && s.Drifted {
			fmt.Fprintln(stderr, "seed plugin status --check:", s.Detail)
			return 1
		}
		if check {
			fmt.Fprintln(stdout, "plugin ok:", s.Detail)
			return 0
		}
		return emit("plugin-status", s, nil)
	default:
		fmt.Fprintf(stderr, "seed: unknown plugin subcommand %q\n", args[0])
		return exitUsage
	}
}
