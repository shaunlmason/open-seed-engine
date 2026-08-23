// Package plan parses and lints plan files (open-seed D3): thin, mandatory,
// gated, pinned. A plan is markdown with four required sections; its
// ## Validation Commands are executed mechanically by the loop runner, the
// pre-merge gate, and CI verify, always from the merge-base blob, never the
// PR head's copy.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

var RequiredSections = []string{"Steps", "File Scope", "Acceptance Criteria", "Validation Commands"}

type Plan struct {
	Sections           map[string]string
	ValidationCommands []string
}

// Hash is the plan pin: sha256 of the exact blob (D3 merge-base rule).
func Hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Parse splits ## sections and extracts validation commands from bullets
// (`- cmd`, backticks stripped) and fenced code blocks in that section.
func Parse(content string) *Plan {
	p := &Plan{Sections: map[string]string{}}
	var current string
	var buf strings.Builder
	flush := func() {
		if current != "" {
			p.Sections[current] = strings.TrimSpace(buf.String())
		}
		buf.Reset()
	}
	for _, line := range strings.Split(content, "\n") {
		if name, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			current = strings.TrimSpace(name)
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()

	section := p.Sections["Validation Commands"]
	inFence := false
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		var cmd string
		switch {
		case inFence && trimmed != "":
			cmd = trimmed
		case strings.HasPrefix(trimmed, "- "):
			cmd = strings.Trim(strings.TrimPrefix(trimmed, "- "), "`")
		}
		if cmd != "" && !strings.HasPrefix(cmd, "#") {
			p.ValidationCommands = append(p.ValidationCommands, cmd)
		}
	}
	return p
}

// Lint enforces the D3 grammar: all required sections present and non-empty,
// and at least one validation command.
func Lint(content string) []error {
	p := Parse(content)
	var errs []error
	for _, s := range RequiredSections {
		if strings.TrimSpace(p.Sections[s]) == "" {
			errs = append(errs, fmt.Errorf("missing or empty section: ## %s", s))
		}
	}
	if len(p.ValidationCommands) == 0 && strings.TrimSpace(p.Sections["Validation Commands"]) != "" {
		errs = append(errs, fmt.Errorf("## Validation Commands contains no parseable commands (use `- cmd` bullets or a fenced block)"))
	} else if len(p.ValidationCommands) == 0 {
		errs = append(errs, fmt.Errorf("no validation commands — the gates execute these mechanically; a plan without them is unverifiable"))
	}
	return errs
}
