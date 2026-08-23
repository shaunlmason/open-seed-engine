// Package skills implements the D8 shared-skills story (plan
// os-6f3104db): seed.yaml names skill sources, seed.lock pins them
// (commit SHA + content sha256, skillfold semantics), and
// `seed skills install --frozen` makes the pins load-bearing in CI.
// Compose is the same mechanism pointed at multiple sources: skills
// install side by side under skills/managed/ and flow through the
// existing `seed sync` fan-out. Install prunes ONLY the managed
// directory: local skills outside it are never touched. Skill updates
// arrive as ordinary PRs whose diff shows the new content; injection
// review happens in the review pane, never at install time.
package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ManifestPath = "seed.yaml"
	LockPath     = "seed.lock"
	ManagedDir   = "skills/managed"
)

type Source struct {
	Name string `yaml:"name" json:"name"`
	Repo string `yaml:"repo" json:"repo"`
	Ref  string `yaml:"ref" json:"ref"`
	Path string `yaml:"path" json:"path"`
}

// Compose declares a *generated* skill (skillfold semantics,
// inspirations/05): its SKILL.md concatenates the used skills' bodies in
// declared order, headings demoted under a root heading, with supporting
// files carried over. Composed skills are not locked: they are
// deterministic functions of locked inputs, regenerated at install.
type Compose struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Use         []string `yaml:"use" json:"use"`
}

type Manifest struct {
	SchemaVersion string    `yaml:"schema_version"`
	Skills        []Source  `yaml:"skills"`
	Compose       []Compose `yaml:"compose"`
}

type LockEntry struct {
	Source
	Commit string `json:"commit"`
	SHA256 string `json:"sha256"`
}

type Lock struct {
	SchemaVersion string      `json:"schema_version"`
	Skills        []LockEntry `json:"skills"`
}

func refuse(msg string, a ...any) error { return fmt.Errorf(msg, a...) }

// LoadManifest parses seed.yaml strictly (unknown keys refused). A
// missing file is an empty manifest: fresh instantiations pay nothing.
func LoadManifest(root string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(root, ManifestPath))
	if err != nil {
		if os.IsNotExist(err) {
			return &Manifest{SchemaVersion: "1"}, nil
		}
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if err.Error() == "EOF" { // comments-only file
			return &Manifest{SchemaVersion: "1"}, nil
		}
		return nil, refuse("%s: %v", ManifestPath, err)
	}
	if m.SchemaVersion != "1" {
		return nil, refuse("%s schema_version %q is not \"1\"", ManifestPath, m.SchemaVersion)
	}
	seen := map[string]bool{}
	for _, s := range m.Skills {
		if s.Name == "" || s.Repo == "" || s.Ref == "" {
			return nil, refuse("%s: every skill needs name, repo, ref", ManifestPath)
		}
		if strings.ContainsAny(s.Name, "/\\") {
			return nil, refuse("%s: skill name %q may not contain path separators", ManifestPath, s.Name)
		}
		if seen[s.Name] {
			return nil, refuse("%s: duplicate skill name %q", ManifestPath, s.Name)
		}
		seen[s.Name] = true
	}
	for _, c := range m.Compose {
		if c.Name == "" || len(c.Use) == 0 {
			return nil, refuse("%s: every compose entry needs a name and a non-empty use list", ManifestPath)
		}
		if strings.ContainsAny(c.Name, "/\\") {
			return nil, refuse("%s: compose name %q may not contain path separators", ManifestPath, c.Name)
		}
		if seen[c.Name] {
			return nil, refuse("%s: duplicate name %q (compose and skill names share one namespace)", ManifestPath, c.Name)
		}
		seen[c.Name] = true
	}
	// Every use entry must exist (a skill removed while still used is a
	// refusal), self-use is refused, and cycles are refused.
	for _, c := range m.Compose {
		for _, u := range c.Use {
			if u == c.Name {
				return nil, refuse("%s: compose %q uses itself", ManifestPath, c.Name)
			}
			if !seen[u] {
				return nil, refuse("%s: compose %q uses %q, which is not a declared skill or compose entry", ManifestPath, c.Name, u)
			}
		}
	}
	if _, err := composeOrder(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// composeOrder returns compose names in topological (dependency-first)
// order: compose-of-compose is allowed; a cycle is a refusal naming its
// path.
func composeOrder(m *Manifest) ([]string, error) {
	byName := map[string]*Compose{}
	for i := range m.Compose {
		byName[m.Compose[i].Name] = &m.Compose[i]
	}
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := map[string]int{}
	var order []string
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		c, isCompose := byName[name]
		if !isCompose {
			return nil // fetched skill: a leaf
		}
		switch state[name] {
		case done:
			return nil
		case visiting:
			return refuse("%s: compose cycle detected: %s", ManifestPath, strings.Join(append(path, name), " -> "))
		}
		state[name] = visiting
		for _, u := range c.Use {
			if err := visit(u, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		order = append(order, name)
		return nil
	}
	for _, c := range m.Compose {
		if err := visit(c.Name, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func LoadLock(root string) (*Lock, error) {
	raw, err := os.ReadFile(filepath.Join(root, LockPath))
	if err != nil {
		if os.IsNotExist(err) {
			return &Lock{SchemaVersion: "1"}, nil
		}
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, refuse("%s: %v", LockPath, err)
	}
	return &l, nil
}

func gitURL(repo string) string {
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, ".") {
		return repo
	}
	return "https://github.com/" + repo + ".git"
}

// fetchTree fetches ref from the source and returns (commit, path->blob
// content) for the skill subtree.
func fetchTree(workDir, repo, ref, sub string) (string, map[string][]byte, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimRight(string(out), "\n"), nil
	}
	if _, err := git("init", "-q", "--bare", "."); err != nil {
		return "", nil, err
	}
	if _, err := git("fetch", "--no-tags", "--depth", "1", gitURL(repo), ref); err != nil {
		return "", nil, refuse("skill source %s@%s unreachable or ref missing: %v", repo, ref, err)
	}
	commit, err := git("rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", nil, err
	}
	args := []string{"ls-tree", "-r", "--name-only", commit}
	if sub != "" {
		args = append(args, "--", sub)
	}
	listing, err := git(args...)
	if err != nil {
		return "", nil, refuse("skill path %q missing at %s@%s: %v", sub, repo, ref, err)
	}
	files := map[string][]byte{}
	for _, name := range strings.Split(listing, "\n") {
		if name == "" {
			continue
		}
		cmd := exec.Command("git", "cat-file", "-p", commit+":"+name)
		cmd.Dir = workDir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		content, err := cmd.Output()
		if err != nil {
			return "", nil, refuse("git cat-file %s:%s: %v: %s", commit, name, err, strings.TrimSpace(stderr.String()))
		}
		key := name
		if sub != "" {
			key = strings.TrimPrefix(name, strings.TrimSuffix(sub, "/")+"/")
		}
		files[key] = content
	}
	if len(files) == 0 {
		return "", nil, refuse("skill path %q at %s@%s contains no files", sub, repo, ref)
	}
	return commit, files, nil
}

// TreeHash is the file-order-independent content hash: sorted
// path\0sha256(content)\0 pairs.
func TreeHash(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		c := sha256.Sum256(files[n])
		fmt.Fprintf(h, "%s\x00%s\x00", n, hex.EncodeToString(c[:]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashDir(dir string) (string, error) {
	files := map[string][]byte{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = content
		return nil
	})
	if err != nil {
		return "", err
	}
	return TreeHash(files), nil
}

// LockAll resolves every manifest source and rewrites seed.lock.
func LockAll(root string) (*Lock, error) {
	m, err := LoadManifest(root)
	if err != nil {
		return nil, err
	}
	lock := &Lock{SchemaVersion: "1", Skills: []LockEntry{}}
	for _, s := range m.Skills {
		tmp, err := os.MkdirTemp("", "seed-skill-*")
		if err != nil {
			return nil, err
		}
		commit, files, err := fetchTree(tmp, s.Repo, s.Ref, s.Path)
		os.RemoveAll(tmp)
		if err != nil {
			return nil, err
		}
		lock.Skills = append(lock.Skills, LockEntry{Source: s, Commit: commit, SHA256: TreeHash(files)})
	}
	b, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(root, LockPath), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return lock, nil
}

type InstallReport struct {
	Installed []string `json:"installed"`
	Pruned    []string `json:"pruned"`
	Frozen    bool     `json:"frozen"`
}

// Install materializes every locked skill into skills/managed/<name>,
// verifying the tree sha256 (mismatch = refusal). --frozen additionally
// refuses an unlocked manifest edit or on-disk drift. Pruning removes
// ONLY managed entries absent from the lock.
func Install(root string, frozen bool) (*InstallReport, error) {
	m, err := LoadManifest(root)
	if err != nil {
		return nil, err
	}
	lock, err := LoadLock(root)
	if err != nil {
		return nil, err
	}
	if frozen {
		if err := frozenCheck(m, lock); err != nil {
			return nil, err
		}
	}
	rep := &InstallReport{Installed: []string{}, Pruned: []string{}, Frozen: frozen}
	managed := filepath.Join(root, ManagedDir)
	inLock := map[string]bool{}
	for _, e := range lock.Skills {
		inLock[e.Name] = true
		dest := filepath.Join(managed, e.Name)
		// A managed copy already matching its pin is left alone.
		if h, err := hashDir(dest); err == nil && h == e.SHA256 {
			rep.Installed = append(rep.Installed, e.Name+" (up to date)")
			continue
		}
		tmp, err := os.MkdirTemp("", "seed-skill-*")
		if err != nil {
			return nil, err
		}
		commit, files, err := fetchTree(tmp, e.Repo, e.Commit, e.Path)
		os.RemoveAll(tmp)
		if err != nil {
			return nil, err
		}
		if commit != e.Commit {
			return nil, refuse("skill %s: fetched commit %s does not match the lock's %s", e.Name, commit, e.Commit)
		}
		if h := TreeHash(files); h != e.SHA256 {
			return nil, refuse("skill %s: content hash %s does not match the lock's %s — refusing an unverified install (repin with `seed skills lock` if the change is intended)", e.Name, h, e.SHA256)
		}
		if err := os.RemoveAll(dest); err != nil {
			return nil, err
		}
		for name, content := range files {
			full := filepath.Join(dest, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(full, content, 0o644); err != nil {
				return nil, err
			}
		}
		rep.Installed = append(rep.Installed, e.Name)
	}
	// Generate composed skills (dependency-first, so compose-of-compose
	// reads its inputs' already-generated trees).
	order, err := composeOrder(m)
	if err != nil {
		return nil, err
	}
	byName := map[string]Compose{}
	for _, c := range m.Compose {
		byName[c.Name] = c
	}
	for _, name := range order {
		if err := generateCompose(managed, byName[name]); err != nil {
			return nil, err
		}
		inLock[name] = true // composed dirs are managed; pruning keeps them
		rep.Installed = append(rep.Installed, name+" (composed)")
	}
	// Managed-directory-only pruning.
	entries, err := os.ReadDir(managed)
	if err == nil {
		for _, ent := range entries {
			if !inLock[ent.Name()] {
				if err := os.RemoveAll(filepath.Join(managed, ent.Name())); err != nil {
					return nil, err
				}
				rep.Pruned = append(rep.Pruned, ent.Name())
			}
		}
	}
	if frozen {
		// Post-install drift check: on-disk managed trees match their pins.
		for _, e := range lock.Skills {
			h, err := hashDir(filepath.Join(managed, e.Name))
			if err != nil || h != e.SHA256 {
				return nil, refuse("--frozen: managed skill %s drifts from its pin", e.Name)
			}
		}
	}
	return rep, nil
}

// stripFrontmatter drops a leading YAML frontmatter block.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[4:]
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		return rest[i+5:]
	}
	return s
}

// demote nests a part's headings one level under the composed root
// heading; fenced code blocks are untouched.
func demote(s string) string {
	lines := strings.Split(s, "\n")
	fenced := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if !fenced && strings.HasPrefix(line, "#") {
			lines[i] = "#" + line
		}
	}
	return strings.Join(lines, "\n")
}

// generateCompose materializes one composed skill from its inputs'
// managed trees: a generated SKILL.md (root heading + demoted bodies in
// declared order) plus the inputs' supporting files, carried over so
// relative paths keep working.
func generateCompose(managed string, c Compose) error {
	dest := filepath.Join(managed, c.Name)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", c.Name)
	fmt.Fprintf(&b, "\n> Generated by `seed skills install` from: %s. Do not edit — edit the inputs.\n", strings.Join(c.Use, ", "))
	if c.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", c.Description)
	}
	for _, u := range c.Use {
		src := filepath.Join(managed, u)
		body, err := os.ReadFile(filepath.Join(src, "SKILL.md"))
		if err != nil {
			return refuse("compose %s: input %q has no installed SKILL.md (run `seed skills install` with it locked first): %v", c.Name, u, err)
		}
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(demote(stripFrontmatter(string(body))), "\n"))
		err = filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			if rel == "SKILL.md" {
				return nil
			}
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			full := filepath.Join(dest, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			return os.WriteFile(full, content, 0o644)
		})
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte(b.String()), 0o644)
}

// frozenCheck refuses when seed.yaml and seed.lock disagree: an
// unlocked manifest edit.
func frozenCheck(m *Manifest, lock *Lock) error {
	locked := map[string]LockEntry{}
	for _, e := range lock.Skills {
		locked[e.Name] = e
	}
	if len(m.Skills) != len(lock.Skills) {
		return refuse("--frozen: seed.yaml lists %d skill(s) but seed.lock pins %d — run `seed skills lock` and commit the result", len(m.Skills), len(lock.Skills))
	}
	for _, s := range m.Skills {
		e, ok := locked[s.Name]
		if !ok || e.Repo != s.Repo || e.Ref != s.Ref || e.Path != s.Path {
			return refuse("--frozen: skill %q in seed.yaml is not pinned by seed.lock (or its source changed) — run `seed skills lock` and commit the result", s.Name)
		}
	}
	return nil
}
