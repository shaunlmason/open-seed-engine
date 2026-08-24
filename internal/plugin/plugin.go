// Package plugin renders the Claude Code plugin/marketplace channel
// (open-seed §10 Q4, R8, plan os-221f5929): the second distribution path
// for the evolving parts, carrying capabilities while the template repo
// carries structure.
//
// The channel is a GENERATED FAN-OUT, exactly like .claude/agents/ and
// .agents/skills/ already are: `seed sync` renders plugin/** and the root
// .claude-plugin/marketplace.json from the same sources, and
// `seed sync --check` fails offline on drift. That is deliberate: the card
// requires one drift-detection story shared with the template channel, not
// a second mechanism invented for plugins.
//
// The version and the marketplace coordinates come from the single
// .seed/template.lock the template channel already uses (plan os-23494e11),
// so one release bump moves both paths. The marketplace ENTRY carries no
// version: the Claude Code docs warn that plugin.json always wins and a
// stale entry version silently masks it, so the version lives in exactly
// one place.
package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/template"
)

const (
	// PluginName is both the plugin id and the marketplace id: users see
	// `open-seed@open-seed` when they enable the channel.
	PluginName = "open-seed"
	// MarketplaceName is the public marketplace id. Each user may register
	// only one marketplace per name.
	MarketplaceName = "open-seed"
	// Dir is the plugin root inside the template repo. The marketplace
	// entry names it as a relative source, resolved against the marketplace
	// root (the directory holding .claude-plugin/).
	Dir = "plugin"
	// MarketplacePath is the catalog Claude Code reads when the repo is
	// added as a marketplace.
	MarketplacePath = ".claude-plugin/marketplace.json"
	// ManifestPath is the plugin manifest, inside the plugin root.
	ManifestPath = Dir + "/.claude-plugin/plugin.json"

	description   = "open-seed's evolving capabilities: agent skills and role definitions, distributed as a plugin so an instantiated repo can take capability updates without a template merge (R8)."
	marketplaceOf = "open-seed's own capability channel: the plugin half of the two distribution paths, alongside the template repo itself."
)

// File is one rendered file: a repo-relative path and its exact content.
// sync converts these into its own action type; plugin deliberately does
// not import sync (sync imports this package).
type File struct {
	Path    string
	Content string
}

type person struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// manifest is .claude-plugin/plugin.json. Field order is the emitted key
// order: encoding/json follows struct order, so the render is deterministic
// without a custom marshaller.
type manifest struct {
	Schema      string   `json:"$schema"`
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      person   `json:"author"`
	Homepage    string   `json:"homepage"`
	Repository  string   `json:"repository"`
	License     string   `json:"license"`
	Keywords    []string `json:"keywords"`
}

type entry struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Description string `json:"description"`
}

type catalog struct {
	Name        string  `json:"name"`
	Owner       person  `json:"owner"`
	Description string  `json:"description"`
	Plugins     []entry `json:"plugins"`
}

// Coordinates are the channel's identity, read from .seed/template.lock so
// the plugin channel and the template channel can never name different
// releases of the same repo.
type Coordinates struct {
	Repo    string // owner/name
	Version string // the release tag, e.g. v0.1.0
	Owner   string // the owner half of Repo
}

// Load reads the shared coordinates from .seed/template.lock.
func Load(root string) (*Coordinates, error) {
	repo, version, err := template.Provenance(root)
	if err != nil {
		return nil, err
	}
	owner := repo
	if i := strings.Index(repo, "/"); i > 0 {
		owner = repo[:i]
	}
	return &Coordinates{Repo: repo, Version: version, Owner: owner}, nil
}

// Files renders every file the channel owns, sorted by path. Returns nil
// when the repo has no .seed/template.lock: a repo without template
// provenance has no channel to publish, and sync must stay a no-op there
// rather than inventing coordinates.
func Files(root string) ([]File, error) {
	c, err := Load(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var files []File

	m := manifest{
		Schema:      "https://json.schemastore.org/claude-code-plugin-manifest.json",
		Name:        PluginName,
		DisplayName: "open-seed",
		Version:     strings.TrimPrefix(c.Version, "v"),
		Description: description,
		Author:      person{Name: c.Owner, URL: "https://github.com/" + c.Owner},
		Homepage:    "https://github.com/" + c.Repo,
		Repository:  "https://github.com/" + c.Repo,
		License:     "MIT",
		Keywords:    []string{"open-seed", "orchestration", "agents", "skills", "task-tracking"},
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	files = append(files, File{Path: ManifestPath, Content: string(b) + "\n"})

	cat := catalog{
		Name:        MarketplaceName,
		Owner:       person{Name: c.Owner, URL: "https://github.com/" + c.Owner},
		Description: marketplaceOf,
		Plugins: []entry{{
			Name:        PluginName,
			Source:      "./" + Dir,
			Description: description,
		}},
	}
	b, err = json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return nil, err
	}
	files = append(files, File{Path: MarketplacePath, Content: string(b) + "\n"})

	skillFiles, err := skillPayload(root)
	if err != nil {
		return nil, err
	}
	files = append(files, skillFiles...)

	roleFiles, err := rolePayload(root)
	if err != nil {
		return nil, err
	}
	files = append(files, roleFiles...)

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// skillPayload copies skills/<name>/** into the plugin, EXCLUDING
// skills/managed/. Managed skills are third-party content pinned by the
// consumer's own seed.yaml/seed.lock (plan os-6f3104db): republishing them
// under open-seed's manifest would misattribute provenance and route
// around the lock's hash pinning.
func skillPayload(root string) ([]File, error) {
	src := filepath.Join(root, "skills")
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []File
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "managed" {
			continue
		}
		err := filepath.WalkDir(filepath.Join(src, e.Name()), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files = append(files, File{Path: filepath.Join(Dir, "skills", rel), Content: string(b)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// rolePayload copies .seed/agents/*.md byte-identically into the plugin.
// D8 binds the role sources to be dual-format (Claude Code subagent fields
// alongside seed's run-agent/permission) and "fanned out unchanged": the
// plugin is one more unchanged fan-out, never a transformed one.
func rolePayload(root string) ([]File, error) {
	dir := filepath.Join(root, ".seed", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: filepath.Join(Dir, "agents", e.Name()), Content: string(b)})
	}
	return files, nil
}

func (c *Coordinates) String() string { return fmt.Sprintf("%s@%s", c.Repo, c.Version) }
