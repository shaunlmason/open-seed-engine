// Package sync generates the per-harness fan-outs from the source trees
// (open-seed R1): .seed/agents/ → .claude/agents/, skills/ → .claude/skills/
// and .agents/skills/, and rules/ fragments → the AGENTS.md managed block.
// Fan-out copies are byte-identical to their sources (role frontmatter must
// stay first, so no injected headers: the README markers in each fan-out
// dir carry the "do not edit here" warning). `--check` recomputes offline
// and fails on drift; it runs in CI so the fan-out can never diverge
// silently.
package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	rulesBegin = "<!-- seed:rules:begin"
	rulesEnd   = "<!-- seed:rules:end -->"
)

// Action is one desired file state (relative path → content).
type Action struct {
	Path    string
	Content string
}

// Plan computes every generated file's desired content.
func Plan(root string) ([]Action, error) {
	var actions []Action

	roleActions, err := copyTree(root, filepath.Join(".seed", "agents"), []string{filepath.Join(".claude", "agents")}, ".md", "README.md")
	if err != nil {
		return nil, err
	}
	actions = append(actions, roleActions...)

	skillActions, err := copySkills(root)
	if err != nil {
		return nil, err
	}
	actions = append(actions, skillActions...)

	agentsMD, err := renderAgentsMD(root)
	if err != nil {
		return nil, err
	}
	if agentsMD != "" {
		actions = append(actions, Action{Path: "AGENTS.md", Content: agentsMD})
	}

	sort.Slice(actions, func(i, j int) bool { return actions[i].Path < actions[j].Path })
	return actions, nil
}

func copyTree(root, srcDir string, dstDirs []string, suffix, skip string) ([]Action, error) {
	entries, err := os.ReadDir(filepath.Join(root, srcDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var actions []Action
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) || e.Name() == skip {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, srcDir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, d := range dstDirs {
			actions = append(actions, Action{Path: filepath.Join(d, e.Name()), Content: string(b)})
		}
	}
	return actions, nil
}

// copySkills fans out each skills/<name>/ directory (all files) to both
// harness locations. Managed skills (skills/managed/<name>, installed by
// `seed skills install`: plan os-6f3104db) fan out exactly like local
// ones: the managed/ segment is stripped so harnesses discover them at
// the same depth. Managed entries are emitted first, so a local skill
// with the same name wins.
func copySkills(root string) ([]Action, error) {
	src := filepath.Join(root, "skills")
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var actions []Action
	emit := func(dir, name string) error {
		return filepath.WalkDir(filepath.Join(dir, name), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, dst := range []string{filepath.Join(".claude", "skills"), filepath.Join(".agents", "skills")} {
				actions = append(actions, Action{Path: filepath.Join(dst, rel), Content: string(b)})
			}
			return nil
		})
	}
	managed := filepath.Join(src, "managed")
	if mEntries, err := os.ReadDir(managed); err == nil {
		for _, e := range mEntries {
			if !e.IsDir() {
				continue
			}
			if err := emit(managed, e.Name()); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "managed" {
			continue
		}
		if err := emit(src, e.Name()); err != nil {
			return nil, err
		}
	}
	return actions, nil
}

// renderAgentsMD replaces the managed rules block in AGENTS.md with the
// concatenated rules/ fragments (filename order, each fragment's leading H1
// dropped). Returns "" when AGENTS.md or its markers are absent.
func renderAgentsMD(root string) (string, error) {
	agentsPath := filepath.Join(root, "AGENTS.md")
	current, err := os.ReadFile(agentsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	content := string(current)
	beginIdx := strings.Index(content, rulesBegin)
	endIdx := strings.Index(content, rulesEnd)
	if beginIdx < 0 || endIdx < 0 || endIdx < beginIdx {
		return "", nil
	}
	beginLineEnd := strings.Index(content[beginIdx:], "\n")
	if beginLineEnd < 0 {
		return "", nil
	}
	head := content[:beginIdx+beginLineEnd+1]
	tail := content[endIdx:]

	fragments, err := ruleFragments(root)
	if err != nil {
		return "", err
	}
	if fragments == "" {
		return "", nil
	}
	return head + fragments + tail, nil
}

func ruleFragments(root string) (string, error) {
	dir := filepath.Join(root, "rules")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return "", err
		}
		body := string(b)
		if strings.HasPrefix(body, "# ") {
			if i := strings.Index(body, "\n"); i >= 0 {
				body = body[i+1:]
			}
		}
		body = strings.TrimSpace(body)
		if body != "" {
			parts = append(parts, body)
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n\n") + "\n", nil
}

// Apply writes every generated file.
func Apply(root string) (int, error) {
	actions, err := Plan(root)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, a := range actions {
		full := filepath.Join(root, a.Path)
		existing, err := os.ReadFile(full)
		if err == nil && string(existing) == a.Content {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// Check reports drift between sources and generated files (offline).
func Check(root string) []error {
	actions, err := Plan(root)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, a := range actions {
		existing, err := os.ReadFile(filepath.Join(root, a.Path))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: missing (run seed sync)", a.Path))
			continue
		}
		if string(existing) != a.Content {
			errs = append(errs, fmt.Errorf("%s: drifted from its source (run seed sync; edit the source tree, never the fan-out — R1)", a.Path))
		}
	}
	return errs
}
