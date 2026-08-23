package backend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaunlmason/open-seed-engine/internal/spec"
)

const manifest = `name = "echo"
version = "0.0.1"
schema_version = "1.0"
entry = "bin/seed-backend"
requires_env = ["ECHO_SECRET"]
[capabilities]
required = true
atomic_claim = "native"
offline = "native"
budget = "none"
`

// The fake plugin prints its argv and selected env as an envelope; verb
// "contend" exits 2, verb "garbage" prints non-JSON.
const plugin = `#!/bin/sh
case "$1" in
  garbage) echo "not json"; exit 0 ;;
  contend) printf '{"ok":false,"schema_version":"1.0","error":"claim_contention"}\n'; exit 2 ;;
  *) printf '{"ok":true,"schema_version":"1.0","argv":"%s","secret":"%s","leak":"%s"}\n' "$*" "${ECHO_SECRET:-}" "${LEAKY:-}" ;;
esac
`

func setup(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".seed", "backends", "echo")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "backend.toml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(dir, "bin", "seed-backend"), []byte(plugin), 0o755)
	writeLock(t, root, map[string]lockEntry{"echo": {Source: "in-template"}})
	return root
}

func writeLock(t *testing.T, root string, lock map[string]lockEntry) {
	t.Helper()
	b, _ := json.Marshal(lock)
	if err := os.WriteFile(filepath.Join(root, ".seed", "backends.lock.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExecRoutesAndPassesExitCodes(t *testing.T) {
	root := setup(t)
	m, err := Load(root, "echo")
	if err != nil {
		t.Fatal(err)
	}
	out, code, err := Exec(root, m, []string{"claim", "os-1", "--actor", "a", "--json"})
	if err != nil || code != 0 {
		t.Fatalf("exec: %v code=%d", err, code)
	}
	if !strings.Contains(out, `"argv":"claim os-1 --actor a --json"`) {
		t.Fatalf("argv not routed: %s", out)
	}
	_, code, err = Exec(root, m, []string{"contend"})
	if err != nil || code != 2 {
		t.Fatalf("contention passthrough: %v code=%d", err, code)
	}
}

func TestExecEnvIsMinimal(t *testing.T) {
	root := setup(t)
	m, _ := Load(root, "echo")
	os.Setenv("ECHO_SECRET", "s3cret")
	os.Setenv("LEAKY", "must-not-appear")
	defer os.Unsetenv("ECHO_SECRET")
	defer os.Unsetenv("LEAKY")
	out, _, err := Exec(root, m, []string{"ping"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"secret":"s3cret"`) {
		t.Errorf("requires_env not injected: %s", out)
	}
	if !strings.Contains(out, `"leak":""`) {
		t.Errorf("environment leaked beyond requires_env: %s", out)
	}
}

func TestSchemaInvalidOutputDiscarded(t *testing.T) {
	root := setup(t)
	m, _ := Load(root, "echo")
	out, code, err := Exec(root, m, []string{"garbage"})
	if err == nil || code != spec.ExitVersionMismatch {
		t.Fatalf("garbage accepted: out=%q code=%d err=%v", out, code, err)
	}
	if out != "" {
		t.Fatalf("invalid output not discarded: %q", out)
	}
}

func TestLockEnforcement(t *testing.T) {
	root := setup(t)
	m, _ := Load(root, "echo")
	dir := filepath.Join(root, ".seed", "backends", "echo")

	// Correct hash pins pass.
	hash, err := HashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeLock(t, root, map[string]lockEntry{"echo": {Source: "github:x/y", SHA256: hash}})
	if _, code, err := Exec(root, m, []string{"ping"}); err != nil || code != 0 {
		t.Fatalf("pinned exec: %v code=%d", err, code)
	}

	// A tampered plugin is refused.
	os.WriteFile(filepath.Join(dir, "bin", "seed-backend"), []byte(plugin+"# tampered\n"), 0o755)
	if _, _, err := Exec(root, m, []string{"ping"}); err == nil || !strings.Contains(err.Error(), "does not match lock") {
		t.Fatalf("tampered plugin invoked: %v", err)
	}

	// No lock entry at all is refused.
	writeLock(t, root, map[string]lockEntry{})
	if _, _, err := Exec(root, m, []string{"ping"}); err == nil || !strings.Contains(err.Error(), "no backends.lock.json entry") {
		t.Fatalf("unpinned plugin invoked: %v", err)
	}

	// External source without a hash is refused.
	writeLock(t, root, map[string]lockEntry{"echo": {Source: "github:x/y"}})
	if _, _, err := Exec(root, m, []string{"ping"}); err == nil || !strings.Contains(err.Error(), "no sha256") {
		t.Fatalf("hashless external plugin invoked: %v", err)
	}
}

func TestLoadManifestRefusals(t *testing.T) {
	root := t.TempDir()
	write := func(c string) {
		p := filepath.Join(root, ".seed", "backends", "m", "backend.toml")
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(root, "m"); err == nil {
		t.Fatal("missing manifest loaded")
	}
	write("name = \"other\"\nschema_version = \"1.0\"\nentry = \"run\"\n[capabilities]\nrequired = true\n")
	if _, err := Load(root, "m"); err == nil {
		t.Fatal("name-mismatched manifest loaded")
	}
	write("name = \"m\"\nschema_version = \"1.0\"\n[capabilities]\nrequired = true\n")
	if _, err := Load(root, "m"); err == nil {
		t.Fatal("entry-less manifest loaded")
	}
	write("name = \"m\"\nschema_version = \"1.0\"\nentry = \"run\"\n[capabilities]\nrequired = false\n")
	if _, err := Load(root, "m"); err == nil {
		t.Fatal("required=false manifest loaded")
	}
	write("name = \"m\"\nschema_version = \"1.0\"\nentry = \"run\"\n[capabilities]\nrequired = true\n")
	if _, err := Load(root, "m"); err != nil {
		t.Fatalf("good manifest refused: %v", err)
	}
}

func TestValidateEnvelopeShapes(t *testing.T) {
	good := `{"ok": true, "schema_version": "1.0"}`
	if err := validateEnvelope(good); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"not json":        "nope",
		"missing ok":      `{"schema_version": "1.0"}`,
		"bad schema":      `{"ok": true, "schema_version": "9.9"}`,
		"missing version": `{"ok": false}`,
	} {
		if err := validateEnvelope(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestVerifyLockAndHashDirErrors(t *testing.T) {
	root := t.TempDir()
	if err := VerifyLock(root, "x"); err == nil {
		t.Fatal("missing lock file tolerated")
	}
	lock := filepath.Join(root, ".seed", "backends.lock.json")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLock(root, "x"); err == nil {
		t.Fatal("unparseable lock tolerated")
	}
	// A sha-pinned entry whose plugin directory is absent.
	if err := os.WriteFile(lock, []byte(`{"x": {"source": "git", "sha256": "deadbeef"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLock(root, "x"); err == nil {
		t.Fatal("missing plugin dir hashed")
	}
	// Present but drifted from the pin.
	pdir := filepath.Join(root, ".seed", "backends", "x")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := VerifyLock(root, "x")
	if err == nil || !strings.Contains(err.Error(), "does not match lock") {
		t.Fatalf("hash drift tolerated: %v", err)
	}
	// The executable bit participates in the hash.
	h1, err := HashDir(pdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(pdir, "run.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := HashDir(pdir)
	if err != nil || h1 == h2 {
		t.Fatalf("exec bit not hashed: %s vs %s (%v)", h1, h2, err)
	}
}

func TestExecEntryMissingAndUnrunnable(t *testing.T) {
	root := t.TempDir()
	lock := filepath.Join(root, ".seed", "backends.lock.json")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte(`{"p": {"source": "builtin"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Name: "p", Entry: "run.sh"}
	if _, _, err := Exec(root, m, []string{"ready"}); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("phantom entry tolerated: %v", err)
	}
	pdir := filepath.Join(root, ".seed", "backends", "p")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "run.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Exec(root, m, []string{"ready"}); err == nil {
		t.Fatal("non-executable entry ran")
	}
}
