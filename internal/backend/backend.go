// Package backend implements the external-plugin dispatch seam (open-seed
// §7.1 / research/10 §5.3–5.5): when the configured backend's manifest entry
// is not "builtin", port verbs are executed by
// .seed/backends/<name>/<entry> <verb> [args] --json. The shim verifies the
// plugin against its lock entry before every invocation, runs it with a
// minimal environment, validates its stdout against the envelope contract,
// and passes exit codes through unchanged. Plugin output is untrusted input:
// schema-invalid output is discarded with exit 10.
package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/shaunlmason/open-seed-engine/internal/spec"
)

type Capabilities struct {
	Required    bool     `toml:"required"`
	Optional    []string `toml:"optional"`
	AtomicClaim string   `toml:"atomic_claim"`
	Offline     string   `toml:"offline"`
	Budget      string   `toml:"budget"`
	// StatePortability declares where the coordination state travels:
	// "repo" (rides a git ref), "replica" (syncs between machines),
	// "server" (a control plane holds it), "machine" (local only —
	// fastcards). Machine-readable so negotiation can inspect the
	// variance instead of reading README prose.
	StatePortability string `toml:"state_portability"`
}

type Manifest struct {
	Name          string       `toml:"name"`
	Version       string       `toml:"version"`
	SchemaVersion string       `toml:"schema_version"`
	Entry         string       `toml:"entry"`
	Requires      []string     `toml:"requires"`
	RequiresEnv   []string     `toml:"requires_env"`
	Capabilities  Capabilities `toml:"capabilities"`
}

type lockEntry struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// Load reads and minimally validates a plugin manifest.
func Load(root, name string) (*Manifest, error) {
	var m Manifest
	path := filepath.Join(root, ".seed", "backends", name, "backend.toml")
	if _, err := toml.DecodeFile(path, &m); err != nil {
		return nil, fmt.Errorf("backend %s: %w", name, err)
	}
	if m.Name != name {
		return nil, fmt.Errorf("backend %s: manifest names %q", name, m.Name)
	}
	if m.Entry == "" || m.SchemaVersion == "" {
		return nil, fmt.Errorf("backend %s: manifest missing entry/schema_version", name)
	}
	if !m.Capabilities.Required {
		return nil, fmt.Errorf("backend %s: does not declare the full REQUIRED verb set", name)
	}
	return &m, nil
}

// VerifyLock enforces the pin (research/10 §5.5): a lock entry carrying a
// sha256 must match the plugin directory hash exactly; sources "builtin" and
// "in-template" may omit the hash (PR review is the control, stated in the
// lock). A plugin with no lock entry at all is refused.
func VerifyLock(root, name string) error {
	b, err := os.ReadFile(filepath.Join(root, ".seed", "backends.lock.json"))
	if err != nil {
		return fmt.Errorf("backends.lock.json: %w", err)
	}
	var lock map[string]lockEntry
	if err := json.Unmarshal(b, &lock); err != nil {
		return fmt.Errorf("backends.lock.json: %w", err)
	}
	entry, ok := lock[name]
	if !ok {
		return fmt.Errorf("backend %s has no backends.lock.json entry — refusing to invoke an unpinned plugin", name)
	}
	if entry.SHA256 == "" {
		if entry.Source == "builtin" || entry.Source == "in-template" {
			return nil
		}
		return fmt.Errorf("backend %s: lock entry has no sha256 and source %q is not in-template", name, entry.Source)
	}
	got, err := HashDir(filepath.Join(root, ".seed", "backends", name))
	if err != nil {
		return err
	}
	if got != entry.SHA256 {
		return fmt.Errorf("backend %s: directory hash %s does not match lock %s — refusing to invoke (review the diff, then update the lock)", name, got, entry.SHA256)
	}
	return nil
}

// HashDir computes a deterministic sha256 over the plugin directory: sorted
// relative paths, each contributing its path, mode-executable bit, and
// content hash.
func HashDir(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		info, err := os.Stat(f)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		c := sha256.Sum256(content)
		exe := "0"
		if info.Mode()&0o111 != 0 {
			exe = "1"
		}
		fmt.Fprintf(h, "%s|%s|%s\n", filepath.ToSlash(rel), exe, hex.EncodeToString(c[:]))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Exec runs the plugin for one verb. Returns the validated stdout and the
// plugin's exit code. Environment is minimal: PATH and HOME plus the
// manifest's requires_env (least privilege, §5.5).
func Exec(root string, m *Manifest, argv []string) (string, int, error) {
	if err := VerifyLock(root, m.Name); err != nil {
		return "", spec.ExitUnavailable, err
	}
	entry := filepath.Join(root, ".seed", "backends", m.Name, m.Entry)
	if _, err := os.Stat(entry); err != nil {
		return "", spec.ExitUnavailable, fmt.Errorf("backend %s: entry %s not found", m.Name, m.Entry)
	}
	cmd := exec.Command(entry, argv...)
	cmd.Dir = root
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for _, k := range m.RequiresEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			return "", spec.ExitUnavailable, fmt.Errorf("backend %s: %w", m.Name, runErr)
		}
		code = ee.ExitCode()
	}
	out := stdout.String()
	if err := validateEnvelope(out); err != nil {
		return "", spec.ExitVersionMismatch, fmt.Errorf("backend %s produced schema-invalid output (discarded): %v; stderr: %s", m.Name, err, strings.TrimSpace(stderr.String()))
	}
	return out, code, nil
}

// validateEnvelope enforces the response contract before anything reaches an
// agent: one JSON object with boolean ok and a 1.x schema_version.
func validateEnvelope(out string) error {
	var env struct {
		OK            *bool  `json:"ok"`
		SchemaVersion string `json:"schema_version"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&env); err != nil {
		return fmt.Errorf("not a JSON object: %v", err)
	}
	if env.OK == nil {
		return fmt.Errorf("missing ok field")
	}
	if !strings.HasPrefix(env.SchemaVersion, "1.") {
		return fmt.Errorf("missing or unsupported schema_version %q", env.SchemaVersion)
	}
	return nil
}
